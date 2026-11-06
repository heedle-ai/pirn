package pirn_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heedle-ai/pirn"
	"github.com/heedle-ai/reed"
)

// Saved sign-ins per site (roadmap 9.11).

const state = `{"cookies":[{"name":"sid","value":"s3cr3t-cookie","domain":"forms.example.org","path":"/"}],"origins":[{"origin":"https://forms.example.org","localStorage":[{"name":"token","value":"s3cr3t-local"}]}]}`

func newSessions(t *testing.T) (*pirn.Sessions, memVault, *time.Time) {
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	v := memVault{}
	return &pirn.Sessions{Dir: filepath.Join(t.TempDir(), "sessions"), Vault: v, Now: func() time.Time { return now }}, v, &now
}

// TestInv_SessionsAreSealed: a kept sign-in is a file that holds none of
// what it keeps, only the person can read, and that opens only with its
// own key from the vault, for its own host; forgetting removes both the
// file and the key; one unused for 30 days expires, file and key, when
// it is next looked for or listed (roadmap 9.11).
func TestInv_SessionsAreSealed(t *testing.T) {
	ctx := context.Background()
	s, v, now := newSessions(t)
	if err := s.Save(ctx, "forms.example.org", []byte(state)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(s.Dir, "forms.example.org.session")
	b, err := os.ReadFile(file)
	fi, _ := os.Stat(file)
	di, _ := os.Stat(s.Dir)
	if err != nil || strings.Contains(string(b), "s3cr3t") || strings.Contains(string(b), "sid") || fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("the file: %v %s %v %v", err, b, fi.Mode(), di.Mode())
	}
	key := pirn.SessionKey("forms.example.org")
	if _, ok := v[key]; !ok || !pirn.IsSessionName(key) || len(v) != 1 {
		t.Fatalf("the key: %v", v)
	}
	got, ok, err := s.Load(ctx, "forms.example.org")
	if err != nil || !ok || string(got) != state {
		t.Fatalf("load: %v %v %s", ok, err, got)
	}
	// Without its key, or with another, it doesn't open.
	saved := v[key]
	v[key] = strings.Repeat("A", 43)
	if _, ok, err := s.Load(ctx, "forms.example.org"); ok || err != nil {
		t.Errorf("opened with another key: %v %v", ok, err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Error("a file that doesn't open with its key is still there: every step on the site would fail (review 53)")
	}
	os.WriteFile(file, b, 0o600)
	v[key] = saved
	// Without its key (heedle secret delete --all), it is nothing: it
	// doesn't open, and the file is forgotten too.
	delete(v, key)
	if _, ok, err := s.Load(ctx, "forms.example.org"); ok || err != nil {
		t.Errorf("without its key: %v %v", ok, err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Error("a file without its key is still there")
	}
	os.WriteFile(file, b, 0o600)
	v[key] = saved
	// Moved to another host's name, it doesn't open for that host.
	os.WriteFile(filepath.Join(s.Dir, "evil.example.org.session"), []byte(strings.Replace(string(b), `"host":"forms.example.org"`, `"host":"evil.example.org"`, 1)), 0o600)
	v[pirn.SessionKey("evil.example.org")] = saved
	if _, ok, err := s.Load(ctx, "evil.example.org"); ok || err != nil {
		t.Errorf("a session opened for another host: %v %v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "evil.example.org.session")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a file that doesn't open is still there: the site would be stuck")
	}
	// Forget: both the file and the key.
	if err := s.Forget(ctx, "forms.example.org"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Error("the file is still there")
	}
	if _, ok := v[key]; ok {
		t.Error("the key is still there")
	}
	// Thirty days unused: gone, file and key.
	s.Save(ctx, "forms.example.org", []byte(state))
	s.Save(ctx, "shop.example.org", []byte(state))
	*now = now.Add(20 * 24 * time.Hour)
	s.Touch(ctx, "shop.example.org") // used: its thirty days start again
	s.Load(ctx, "forms.example.org") // looked at, not used: no more days
	*now = now.Add(11 * 24 * time.Hour)
	list, err := s.List(ctx)
	if err != nil || len(list) != 1 || list[0].Host != "shop.example.org" {
		t.Errorf("list: %+v %v", list, err)
	}
	if _, ok := v[pirn.SessionKey("forms.example.org")]; ok {
		t.Error("an expired session's key is still there")
	}
	if _, ok, _ := s.Load(ctx, "forms.example.org"); ok {
		t.Error("an expired session loaded")
	}
	// Forget all: every file and key, and only what Sessions wrote
	// (review 53: a mistaken folder would have lost everything in it).
	if err := s.ForgetAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("the folder is still there")
	}
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "notes.txt"), []byte("mine"), 0o644)
	other := pirn.Sessions{Dir: home, Vault: v}
	other.Save(ctx, "forms.example.org", []byte(state))
	if err := other.ForgetAll(ctx); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(home, "notes.txt")); err != nil || string(b) != "mine" {
		t.Error("forgetting every sign-in removed a file that wasn't one")
	}
	// A folder that is a link to elsewhere is refused.
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(t.TempDir(), link)
	if err := (pirn.Sessions{Dir: link, Vault: v}).Save(ctx, "forms.example.org", []byte(state)); err == nil {
		t.Error("kept a sign-in through a link")
	}
	for k := range v {
		if pirn.IsSessionName(k) {
			t.Errorf("%s is still there", k)
		}
	}
	for _, bad := range []string{"", "../x", "Forms.example.org", "a/b", ".example.org", "a..b"} {
		if err := s.Save(ctx, bad, []byte(state)); err == nil {
			t.Errorf("host %q accepted", bad)
		}
	}
}

// TestInv_SessionLoadsOnlyForItsHost: a step starts signed in only on the
// hosts its program names (:signed-in), each one of its own, with a kept
// sign-in, matched exactly: never a parent domain's, a sub-domain's or a
// look-alike's, and never a host its program doesn't name, even one with a
// kept sign-in (review 53: a schedule made before the sign-in was kept
// would have started signed in, unannounced). Only that host's cookies and
// storage go in, and their file leaves the container once the browser
// has them; what they hold is hidden wherever a page shows it; the record
// names the hosts, never what they hold; and a step signed in as the
// person can't send without asking (roadmap 9.11).
func TestInv_SessionLoadsOnlyForItsHost(t *testing.T) {
	ctx := context.Background()
	s, _, now := newSessions(t)
	for _, h := range []string{"forms.example.org", "example.org", "sub.forms.example.org", "notforms.example.org", "cdn.example.org"} {
		st := strings.ReplaceAll(state, "forms.example.org", h)
		if err := s.Save(ctx, h, []byte(st)); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(time.Hour)
	t.Setenv("FAKE_ECHO", "- text: Your session: s3cr3t-cookie")
	// A navigation the hold held carries what the page showed in its
	// path; the record and every beat hide it (review 53, round 3).
	t.Setenv("FAKE_HELD", "GET https://cdn.example.org/contact/s3cr3t-cookie")
	api := newActAPI(t, actTurn("tool_use", browserUse("t1", "browser_snapshot", `{}`)), actTurn("end_turn", `{"type":"text","text":"Done.\nResult: finished"}`))
	a, b := newAct(t, api)
	a.Sessions = s
	var beats []string
	a.OnProgress = func(r pirn.ActResult, _ reed.Frame) {
		j, _ := json.Marshal(r)
		beats = append(beats, string(j))
	}
	res, err := a.Run(ctx, pirn.ActQuestion{Instruction: "x", URL: "https://forms.example.org/volunteer", Allow: []string{"cdn.example.org"}, SignedIn: []string{"forms.example.org"}})
	if err != nil || strings.Join(res.SignedIn, " ") != "forms.example.org" || strings.Join(b.last.signed, " ") != "forms.example.org" || !b.last.dropped {
		t.Fatalf("%v %v, dropped %v", err, res.SignedIn, b.last.dropped)
	}
	var loaded struct {
		Cookies []struct{ Domain string }
		Origins []struct{ Origin string }
	}
	json.Unmarshal(b.last.storage, &loaded)
	if len(loaded.Cookies) != 1 || loaded.Cookies[0].Domain != "forms.example.org" || len(loaded.Origins) != 1 || loaded.Origins[0].Origin != "https://forms.example.org" {
		t.Errorf("loaded %s", b.last.storage)
	}
	out, _ := json.Marshal(res)
	sent, _ := json.Marshal(api.reqs)
	if strings.Contains(string(out), "s3cr3t") || strings.Contains(string(sent), "s3cr3t") || strings.Contains(strings.Join(beats, ""), "s3cr3t") || !strings.Contains(string(out), `"signedIn":["forms.example.org"]`) || !strings.Contains(string(sent), pirn.SignInPlaceholder) {
		t.Errorf("record %s", out)
	}
	if len(res.Held) != 1 || res.Held[0] != "GET https://cdn.example.org/contact/"+pirn.SignInPlaceholder {
		t.Errorf("held %v", res.Held)
	}
	if list, _ := s.List(ctx); list[2].Host != "forms.example.org" || !list[2].Used.Equal(now.UTC()) || !list[0].Used.Before(now.UTC()) {
		t.Errorf("used: %+v", list)
	}
	// A named host with no kept sign-in isn't signed in: the hold still
	// holds navigations to it, so a provider a sign-in hops through needs
	// a sign-in of its own kept there (review 53, round 4).
	if err := s.Forget(ctx, "cdn.example.org"); err != nil {
		t.Fatal(err)
	}
	a, b = newAct(t, newActAPI(t, actTurn("end_turn", `{"type":"text","text":"x"}`)))
	a.Sessions = s
	if res, err := a.Run(ctx, pirn.ActQuestion{Instruction: "x", URL: "https://forms.example.org/", Allow: []string{"cdn.example.org"}, SignedIn: []string{"cdn.example.org", "forms.example.org"}}); err != nil || strings.Join(res.SignedIn, " ") != "forms.example.org" || strings.Join(b.last.signed, " ") != "forms.example.org" {
		t.Errorf("a named host with nothing kept: %v %v, hold %v", err, res.SignedIn, b.last.signed)
	}
	// A program that doesn't name its hosts starts signed out, with a kept
	// sign-in on each of them.
	a, b = newAct(t, newActAPI(t, actTurn("end_turn", `{"type":"text","text":"x"}`)))
	a.Sessions = s
	if res, err := a.Run(ctx, pirn.ActQuestion{Instruction: "x", URL: "https://forms.example.org/", Allow: []string{"cdn.example.org"}}); err != nil || res.SignedIn != nil || b.last.storage != nil {
		t.Errorf("unnamed: %v %v %s", err, res.SignedIn, b.last.storage)
	}
	// Named, but not one of its hosts; or with :send-without-asking:
	// nothing starts.
	for name, q := range map[string]pirn.ActQuestion{
		"another host":    {Instruction: "x", URL: "https://shop.example.org/", SignedIn: []string{"forms.example.org"}},
		"sending unasked": {Instruction: "x", URL: "https://forms.example.org/", SendFreely: true, SignedIn: []string{"forms.example.org"}},
	} {
		a, b = newAct(t, newActAPI(t, actTurn("end_turn", `{"type":"text","text":"x"}`)))
		a.Sessions = s
		if _, err := a.Run(ctx, q); !errors.Is(err, pirn.ErrInvalid) || b.got.URL != "" {
			t.Errorf("%s: %v, browser %v", name, err, b.got)
		}
	}
	// The option, as a program writes it.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"x", "https://forms.example.org/", pirn.SignedIn, "FORMS.example.org forms.example.org"}, "forms.example.org"},
		{[]string{"x", "https://forms.example.org/", "cdn.example.org", pirn.SignedIn, "cdn.example.org forms.example.org"}, "cdn.example.org forms.example.org"},
		{[]string{"x", "https://forms.example.org/", pirn.SignedIn, "shop.example.org"}, "!"},
		{[]string{"x", "https://forms.example.org/", pirn.SignedIn, "forms.example.org", pirn.SendFreely}, "!"},
		{[]string{"x", "https://forms.example.org/", pirn.SignedIn}, "!"},
		{[]string{"x", "https://forms.example.org/", pirn.SignedIn, "forms.example.org."}, "!"},
	} {
		got, ok := pirn.ActSignedIn(c.args)
		if w := strings.Join(got, " "); c.want == "!" && ok || c.want != "!" && (!ok || w != c.want) {
			t.Errorf("%q: %q %v", c.args, w, ok)
		}
	}
	// A storage state cut to a host keeps nothing of any other.
	if _, ok := pirn.ForHost([]byte(`{"cookies":[{"domain":".example.org"}],"origins":[{"origin":"https://evil.example.org"}]}`), "forms.example.org"); ok {
		t.Error("another host's sign-in kept")
	}
}

// TestInv_PersonKeepsTheirSignIn: a person who hands back asking to keep
// their sign-in keeps the site's the browser is on, that host's alone,
// sealed; the record says which host, never what it holds, and Claude is
// told nothing of it; one who doesn't ask keeps nothing (roadmap 9.11).
func TestInv_PersonKeepsTheirSignIn(t *testing.T) {
	ctx := context.Background()
	for _, keep := range []bool{true, false} {
		s, v, _ := newSessions(t)
		api := newActAPI(t, actTurn("tool_use", browserUse("t1", "browser_navigate", `{"url":"https://forms.example.org/volunteer"}`)), actTurn("tool_use", askPerson),
			actTurn("end_turn", `{"type":"text","text":"Done.\nResult: finished"}`))
		a, _ := newAct(t, api)
		a.Sessions = s
		driver(t, &a, func(w *pirn.Wheel) { w.HandBack(keep, "forms.example.org") })
		res, err := a.Run(ctx, pirn.ActQuestion{Instruction: "x", URL: "https://forms.example.org/volunteer"})
		if err != nil {
			t.Fatal(err)
		}
		list, _ := s.List(ctx)
		out, _ := json.Marshal(res)
		sent, _ := json.Marshal(api.reqs)
		if !keep {
			if len(list) != 0 || res.Kept != "" {
				t.Errorf("kept unasked: %+v", list)
			}
			continue
		}
		if len(list) != 1 || list[0].Host != "forms.example.org" || res.Kept != "forms.example.org" || len(v) != 1 {
			t.Fatalf("kept %+v, record %q", list, res.Kept)
		}
		got, _, _ := s.Load(ctx, "forms.example.org")
		if strings.Contains(string(got), "other") || !strings.Contains(string(got), "s3cr3t-cookie") || !strings.Contains(string(got), "s3cr3t-local") {
			t.Errorf("kept %s", got)
		}
		if strings.Contains(string(out), "s3cr3t") || strings.Contains(string(sent), "s3cr3t") {
			t.Error("a sign-in is in the record or Claude's requests")
		}
	}
	// The browser is on a site other than the one the person saw: nothing
	// kept, and the record says why (review 53).
	for name, c := range map[string]struct {
		saw    string
		freely bool
	}{"another site": {"accounts.example.org", false}, "sending unasked": {"forms.example.org", true}} {
		s, _, _ := newSessions(t)
		api := newActAPI(t, actTurn("tool_use", browserUse("t1", "browser_navigate", `{"url":"https://forms.example.org/volunteer"}`)), actTurn("tool_use", askPerson),
			actTurn("end_turn", `{"type":"text","text":"Done.\nResult: finished"}`))
		a, _ := newAct(t, api)
		a.Sessions = s
		driver(t, &a, func(w *pirn.Wheel) { w.HandBack(true, c.saw) })
		res, err := a.Run(ctx, pirn.ActQuestion{Instruction: "x", URL: "https://forms.example.org/volunteer", Allow: []string{"accounts.example.org"}, SendFreely: c.freely})
		if list, _ := s.List(ctx); err != nil || len(list) != 0 || res.Kept != "" || res.NotKept == "" {
			t.Errorf("%s: %v kept %+v, record %q %q", name, err, list, res.Kept, res.NotKept)
		}
	}
	// A keep sent after the hand-back changes nothing.
	var late *pirn.Wheel
	s, _, _ := newSessions(t)
	api := newActAPI(t, actTurn("tool_use", browserUse("t1", "browser_navigate", `{"url":"https://forms.example.org/volunteer"}`)), actTurn("tool_use", askPerson),
		actTurn("end_turn", `{"type":"text","text":"Done.\nResult: finished"}`))
	a, _ := newAct(t, api)
	a.Sessions = s
	driver(t, &a, func(w *pirn.Wheel) { late = w; w.HandBack(false, "") })
	res, _ := a.Run(ctx, pirn.ActQuestion{Instruction: "x", URL: "https://forms.example.org/volunteer"})
	late.HandBack(true, "forms.example.org")
	if list, _ := s.List(ctx); len(list) != 0 || res.Kept != "" {
		t.Errorf("a late keep kept %+v", list)
	}
	// No connector may name a session's key.
	err := pirn.Connectors{{Name: "x", URL: "https://x.example/mcp", TokenEnv: pirn.SessionKey("forms.example.org")}}.Check()
	if err == nil {
		t.Error("a connector named a session's key")
	}
}

// TestInv_SessionsDontRace: steps and the UI keeping, loading, using and
// forgetting the same site's sign-in at once never leave a file that
// doesn't open with its key (review 53).
func TestInv_SessionsDontRace(t *testing.T) {
	ctx := context.Background()
	s, v, _ := newSessions(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	vault := lockedVault{v: v, mu: &mu}
	s.Vault = vault
	for i := 0; i < 20; i++ {
		wg.Add(4)
		go func() { defer wg.Done(); s.Save(ctx, "forms.example.org", []byte(state)) }()
		go func() { defer wg.Done(); s.Load(ctx, "forms.example.org") }()
		go func() { defer wg.Done(); s.Touch(ctx, "forms.example.org") }()
		go func() {
			defer wg.Done()
			if i%5 == 0 {
				s.Forget(ctx, "forms.example.org")
			}
		}()
	}
	wg.Wait()
	// Either nothing is left, file and key, or a file that opens with its
	// key: never one that doesn't (review 53, round 2). Looked at before
	// Load, which forgets one that doesn't open.
	file := filepath.Join(s.Dir, "forms.example.org.session")
	_, statErr := os.Stat(file)
	_, keyOK := v[pirn.SessionKey("forms.example.org")]
	if statErr != nil {
		if keyOK {
			t.Error("a key without its file")
		}
		return
	}
	if got, ok, err := s.Load(ctx, "forms.example.org"); err != nil || !ok || string(got) != state {
		t.Errorf("after racing, a file that doesn't open: %v %v", ok, err)
	}
}

// TestInv_SessionsDontRace_Forced forces the interleaving that broke a
// sign-in, rather than hoping for it (review 53, round 3): one keep
// stores its key and stalls; another keep of the same site runs meanwhile;
// the first then writes its file. Without the lock the file is sealed
// with a key that's been replaced, and never opens again.
func TestInv_SessionsDontRace_Forced(t *testing.T) {
	ctx := context.Background()
	s, v, _ := newSessions(t)
	var mu sync.Mutex
	g := &gateVault{lockedVault: lockedVault{v: v, mu: &mu}, in: make(chan struct{}), release: make(chan struct{})}
	s.Vault = g
	first := make(chan error, 1)
	go func() { first <- s.Save(ctx, "forms.example.org", []byte(state)) }()
	<-g.in // the first keep has stored its key and stalls
	second := make(chan error, 1)
	go func() {
		second <- s.Save(ctx, "forms.example.org", []byte(state))
	}()
	select {
	case err := <-second:
		t.Errorf("a second keep finished while the first held the lock: %v", err)
		second <- err // for the wait below
	case <-time.After(200 * time.Millisecond):
	}
	close(g.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	<-second
	if got, ok, err := s.Load(ctx, "forms.example.org"); err != nil || !ok || string(got) != state {
		t.Errorf("after two keeps at once, the sign-in doesn't open: %v %v", ok, err)
	}
}

// gateVault stalls in its first Store, after storing, until released.
type gateVault struct {
	lockedVault
	stalled     atomic.Bool
	in, release chan struct{}
}

func (g *gateVault) Store(ctx context.Context, n, val string) error {
	err := g.lockedVault.Store(ctx, n, val)
	if g.stalled.CompareAndSwap(false, true) { // only the first; sync.Once would stall the second too
		close(g.in)
		<-g.release
	}
	return err
}

// lockedVault is a memVault safe to use at once.
type lockedVault struct {
	v  memVault
	mu *sync.Mutex
}

func (l lockedVault) Lookup(ctx context.Context, n string) (string, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.v.Lookup(ctx, n)
}
func (l lockedVault) Store(ctx context.Context, n, val string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.v.Store(ctx, n, val)
}
func (l lockedVault) Delete(ctx context.Context, n string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.v.Delete(ctx, n)
}

// TestInv_OnlySaveTightensTheFolder: listing, loading, touching and
// forgetting never create the sign-ins' folder or change its mode, so a
// folder named by mistake keeps its own; only keeping one creates it, or
// makes it the person's alone (review 53, rounds 2 and 3).
func TestInv_OnlySaveTightensTheFolder(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newSessions(t)
	s.List(ctx)
	s.Load(ctx, "forms.example.org")
	s.Touch(ctx, "forms.example.org")
	s.Forget(ctx, "forms.example.org")
	s.ForgetAll(ctx)
	if _, err := os.Stat(s.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("made the folder: %v", err)
	}
	if err := os.Mkdir(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(s.Dir, 0o755)
	os.WriteFile(filepath.Join(s.Dir, "notes.txt"), []byte("mine"), 0o644)
	s.List(ctx)
	s.Load(ctx, "forms.example.org")
	s.Touch(ctx, "forms.example.org")
	s.Forget(ctx, "forms.example.org")
	s.ForgetAll(ctx)
	if fi, err := os.Stat(s.Dir); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the folder's mode changed: %v %v", fi.Mode(), err)
	}
	if err := s.Save(ctx, "forms.example.org", []byte(state)); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(s.Dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("keeping one left it %v", fi.Mode())
	}
}
