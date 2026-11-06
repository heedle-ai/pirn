package pirn

// Saved sign-ins per site (roadmap 9.11). After a person signs in while
// driving (Take the wheel), they may keep that one site's session: the
// browser's storage state (Playwright's own, cookies and local storage)
// for that host alone. It is kept encrypted (AES-256-GCM, Go's standard
// library) in a file of its own under Sessions.Dir, with its key in the
// vault, one key per host, so the file is nothing without the keychain,
// and forgetting removes both. It loads only into act steps whose hosts
// include its host, matched exactly, and a session unused for SessionTTL
// expires.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	// SessionTTL is how long a session may go unused before it expires.
	SessionTTL = 30 * 24 * time.Hour
	// MaxSession bounds a session's storage state.
	MaxSession = 1 << 20
	// sessionPrefix names a session's key in the vault.
	sessionPrefix = "HEEDLE_SESSION_"
)

// Sessions are the saved sign-ins: their files in Dir (heedle up's
// ~/.heedle/sessions), their keys in Vault.
type Sessions struct {
	Dir   string
	Vault ProfileVault
	// Now, if set, is the clock (tests).
	Now func() time.Time
}

// SessionInfo is a saved sign-in as listed: its host, when it was kept
// and when a step last used it. Never its contents.
type SessionInfo struct {
	Host  string    `json:"host"`
	Saved time.Time `json:"saved"`
	Used  time.Time `json:"used"`
}

// sessionFile is a session on disk: the host and times in the clear, the
// storage state sealed with the host's key, the host as associated data
// (a file renamed to another host doesn't open).
type sessionFile struct {
	SessionInfo
	Nonce  string `json:"nonce"`
	Sealed string `json:"sealed"`
}

// IsSessionName reports whether a secret's name is a session's key: no
// connector may name one, and the secret commands leave them be.
func IsSessionName(name string) bool { return strings.HasPrefix(strings.ToUpper(name), sessionPrefix) }

// SessionKey is the vault name of host's key.
func SessionKey(host string) string {
	h := sha256.Sum256([]byte(host))
	return sessionPrefix + strings.ToUpper(hex.EncodeToString(h[:12]))
}

func (s Sessions) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// checkHost refuses anything but a lower-case host name or address, as an
// allowlist holds them.
func checkHost(host string) error {
	if host == "" || len(host) > 253 || strings.Trim(host, "abcdefghijklmnopqrstuvwxyz0123456789.-:[]") != "" || strings.Contains(host, "..") || strings.HasPrefix(host, ".") {
		return fmt.Errorf("%w: not a host: %q", ErrInvalid, host)
	}
	return nil
}

func (s Sessions) path(host string) string { return filepath.Join(s.Dir, host+".session") }

// lock holds the folder's lock file (flock) for one change or reading:
// two steps or a step and the UI never interleave (review 53). Only
// keeping one (make) makes the folder and tightens it to the person
// alone; anything else finds no folder as nothing kept (os.ErrNotExist),
// and changes no folder's permissions. A folder that is a symbolic link is
// refused. The lock file stays in the folder: unlinked while held, another
// waiter could lock a file no one else sees.
func (s Sessions) lock(make bool) (func(), error) {
	if make {
		if err := os.MkdirAll(s.Dir, 0o700); err != nil {
			return nil, err
		}
	}
	fi, err := os.Lstat(s.Dir)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s isn't a folder of its own", ErrSetup, s.Dir)
	}
	if make && fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(s.Dir, 0o700); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// Save keeps state, Playwright's storage state for host alone (ForHost),
// replacing any kept before: a new key in the vault, then the sealed file.
func (s Sessions) Save(ctx context.Context, host string, state []byte) error {
	if err := checkHost(host); err != nil {
		return err
	}
	if len(state) > MaxSession || !json.Valid(state) {
		return fmt.Errorf("%w: a session is a storage state of at most %d bytes", ErrInvalid, MaxSession)
	}
	unlock, err := s.lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	if err := s.Vault.Store(ctx, SessionKey(host), base64.RawURLEncoding.EncodeToString(key)); err != nil {
		return fmt.Errorf("%w: the keychain didn't keep the key", ErrSetup) // never its words
	}
	gcm, err := sealer(key)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	now := s.now().UTC()
	f := sessionFile{SessionInfo: SessionInfo{Host: host, Saved: now, Used: now},
		Nonce: base64.StdEncoding.EncodeToString(nonce), Sealed: base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, state, []byte(host)))}
	return s.write(f)
}

// write puts f in its file atomically, only the person able to read it;
// the lock is held.
func (s Sessions) write(f sessionFile) error {
	b, _ := json.Marshal(f)
	tmp, err := os.CreateTemp(s.Dir, ".session-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path(f.Host))
}

func sealer(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func (s Sessions) read(host string) (sessionFile, bool, error) {
	var f sessionFile
	b, err := os.ReadFile(s.path(host))
	if errors.Is(err, os.ErrNotExist) {
		return f, false, nil
	}
	if err != nil {
		return f, false, err
	}
	if json.Unmarshal(b, &f) != nil || f.Host != host {
		return f, false, fmt.Errorf("%w: the session for %s can't be read", ErrInvalid, host)
	}
	return f, true, nil
}

// Load is host's session, if one is kept and not expired, decrypted with
// its key. One expired, whose key has gone, or that doesn't open (a file
// changed or from elsewhere) is forgotten, and the step starts signed out
// (review 53). Using it is Touch's, once the browser has it.
func (s Sessions) Load(ctx context.Context, host string) ([]byte, bool, error) {
	if checkHost(host) != nil {
		return nil, false, nil
	}
	unlock, err := s.lock(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil // no folder: nothing kept
	}
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	f, ok, err := s.read(host)
	switch {
	case errors.Is(err, ErrInvalid):
		return nil, false, s.forget(ctx, host)
	case err != nil || !ok:
		return nil, false, err
	case s.now().Sub(f.Used) > SessionTTL:
		return nil, false, s.forget(ctx, host)
	}
	k, ok, err := s.Vault.Lookup(ctx, SessionKey(host))
	if err != nil {
		return nil, false, fmt.Errorf("%w: the keychain didn't answer", ErrSetup) // never its words
	}
	key, kerr := base64.RawURLEncoding.DecodeString(k)
	if !ok || kerr != nil || len(key) != 32 {
		// Its key is gone (heedle secret delete --all, say): the file is
		// nothing without it, so it is forgotten too.
		return nil, false, s.forget(ctx, host)
	}
	nonce, _ := base64.StdEncoding.DecodeString(f.Nonce)
	sealed, _ := base64.StdEncoding.DecodeString(f.Sealed)
	gcm, err := sealer(key)
	if err != nil {
		return nil, false, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, false, s.forget(ctx, host)
	}
	state, err := gcm.Open(nil, nonce, sealed, []byte(host))
	if err != nil {
		return nil, false, s.forget(ctx, host)
	}
	return state, true, nil
}

// Touch counts host's session as used now: it has 30 more days.
func (s Sessions) Touch(ctx context.Context, host string) error {
	if checkHost(host) != nil {
		return nil
	}
	unlock, err := s.lock(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil // no folder: nothing kept
	}
	if err != nil {
		return err
	}
	defer unlock()
	f, ok, err := s.read(host) // read again under the lock: never a stale file written back
	if err != nil || !ok {
		return err
	}
	f.Used = s.now().UTC()
	return s.write(f)
}

// Forget removes host's session: its file and its key.
func (s Sessions) Forget(ctx context.Context, host string) error {
	if err := checkHost(host); err != nil {
		return err
	}
	unlock, err := s.lock(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil // no folder: nothing kept
	}
	if err != nil {
		return err
	}
	defer unlock()
	return s.forget(ctx, host)
}

// forget is Forget with the lock held.
func (s Sessions) forget(ctx context.Context, host string) error {
	if err := os.Remove(s.path(host)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := s.Vault.Delete(ctx, SessionKey(host)); err != nil {
		return fmt.Errorf("%w: the keychain didn't forget the key of %s's sign-in", ErrSetup, host) // never its words
	}
	return nil
}

// List is every session kept, by host; ones unused for SessionTTL are
// forgotten first.
func (s Sessions) List(ctx context.Context) ([]SessionInfo, error) {
	if _, err := os.Lstat(s.Dir); errors.Is(err, os.ErrNotExist) {
		return []SessionInfo{}, nil
	}
	unlock, err := s.lock(false)
	if errors.Is(err, os.ErrNotExist) {
		return []SessionInfo{}, nil // no folder: nothing kept
	}
	if err != nil {
		return nil, err
	}
	defer unlock()
	es, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	out := []SessionInfo{}
	for _, e := range es {
		host, ok := strings.CutSuffix(e.Name(), ".session")
		if !ok || checkHost(host) != nil {
			continue
		}
		f, ok, err := s.read(host)
		if errors.Is(err, ErrInvalid) || ok && s.now().Sub(f.Used) > SessionTTL {
			// One that can't be read, or expired, is forgotten.
			if err := s.forget(ctx, host); err != nil {
				return nil, err
			}
			continue
		}
		if ok {
			out = append(out, f.SessionInfo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

// ForgetAll removes every session: each file and its key, the files of
// sessions whose key has gone, and the folder if nothing else is in it.
// It removes only what Sessions writes, never anything else in Dir
// (review 53: a mistaken folder would have lost everything in it).
func (s Sessions) ForgetAll(ctx context.Context) error {
	if _, err := os.Lstat(s.Dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	unlock, err := s.lock(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil // no folder: nothing kept
	}
	if err != nil {
		return err
	}
	es, err := os.ReadDir(s.Dir)
	if err != nil {
		unlock()
		return err
	}
	for _, e := range es {
		name := e.Name()
		if host, ok := strings.CutSuffix(name, ".session"); ok && checkHost(host) == nil {
			if err := s.forget(ctx, host); err != nil {
				unlock()
				return err
			}
		} else if strings.HasPrefix(name, ".session-") {
			os.Remove(filepath.Join(s.Dir, name))
		}
	}
	unlock()
	os.Remove(filepath.Join(s.Dir, ".lock"))
	os.Remove(s.Dir) // only if nothing else is in it
	return nil
}

// ForHost is a storage state cut to host alone, matched exactly: its
// cookies whose domain is host (with or without a leading dot), and the
// local storage of origins on host. Nothing of any other host, not even
// a parent domain's, is kept.
func ForHost(state []byte, host string) ([]byte, bool) {
	var s struct {
		Cookies []map[string]any `json:"cookies"`
		Origins []map[string]any `json:"origins"`
	}
	if json.Unmarshal(state, &s) != nil {
		return nil, false
	}
	out := struct {
		Cookies []map[string]any `json:"cookies"`
		Origins []map[string]any `json:"origins"`
	}{[]map[string]any{}, []map[string]any{}}
	for _, c := range s.Cookies {
		if d, _ := c["domain"].(string); strings.TrimPrefix(strings.ToLower(d), ".") == host {
			out.Cookies = append(out.Cookies, c)
		}
	}
	for _, o := range s.Origins {
		if u, err := url.Parse(fmt.Sprint(o["origin"])); err == nil && strings.ToLower(u.Hostname()) == host {
			out.Origins = append(out.Origins, o)
		}
	}
	if len(out.Cookies) == 0 && len(out.Origins) == 0 {
		return nil, false
	}
	b, _ := json.Marshal(out)
	return b, true
}

// mergeStates is the storage states of several hosts as one.
func mergeStates(states [][]byte) []byte {
	all := struct {
		Cookies []json.RawMessage `json:"cookies"`
		Origins []json.RawMessage `json:"origins"`
	}{[]json.RawMessage{}, []json.RawMessage{}}
	for _, st := range states {
		var s struct {
			Cookies []json.RawMessage `json:"cookies"`
			Origins []json.RawMessage `json:"origins"`
		}
		if json.Unmarshal(st, &s) == nil {
			all.Cookies, all.Origins = append(all.Cookies, s.Cookies...), append(all.Origins, s.Origins...)
		}
	}
	b, _ := json.Marshal(all)
	return b
}
