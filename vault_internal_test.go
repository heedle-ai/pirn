package pirn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fakeVault map[string]string

func (f fakeVault) Lookup(_ context.Context, name string) (string, bool, error) {
	if name == "BROKEN" {
		return "", false, errors.New("locked")
	}
	v, ok := f[name]
	return v, ok, nil
}

// TestInv_SecretsComeFromEnvironmentThenVault: a connector's secret is
// the worker's variable if set, else the vault's; with neither, the step
// fails as a setup error that says how to store it; a vault that can't
// be read is an error, never an empty secret. (Roadmap 7.1.)
func TestInv_SecretsComeFromEnvironmentThenVault(t *testing.T) {
	ctx := context.Background()
	env := map[string]string{"FROM_ENV": "env-value"}
	getenv = func(k string) string { return env[k] }
	t.Cleanup(func() { getenv = os.Getenv })
	v := fakeVault{"FROM_ENV": "vault-value", "MAPS_KEY": "vault-maps"}

	remote := Connector{Name: "maps", URL: "https://maps.example/mcp", TokenEnv: "MAPS_KEY"}
	if tok, err := remote.token(ctx, v); err != nil || tok != "vault-maps" {
		t.Errorf("token from the vault = %q, %v", tok, err)
	}
	if tok, err := (Connector{Name: "x", URL: "https://x.example", TokenEnv: "FROM_ENV"}).token(ctx, v); err != nil || tok != "env-value" {
		t.Errorf("the environment comes first: %q, %v", tok, err)
	}
	local := Connector{Name: "search", Image: "img@sha256:" + strings.Repeat("a", 64), Command: []string{"srv"}, Secrets: map[string]string{"KEY": "MAPS_KEY", "OTHER": "FROM_ENV"}}
	if s, err := local.secrets(ctx, v); err != nil || s["KEY"] != "vault-maps" || s["OTHER"] != "env-value" {
		t.Errorf("local secrets = %v, %v", s, err)
	}
	_, err := (Connector{Name: "maps", URL: "https://m.example", TokenEnv: "MISSING"}).token(ctx, v)
	if !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "heedle secret set MISSING") {
		t.Errorf("missing secret: %v", err)
	}
	if _, err := (Connector{Name: "maps", URL: "https://m.example", TokenEnv: "BROKEN"}).token(ctx, v); err == nil {
		t.Error("a vault that can't be read gave a secret")
	}
	if _, err := (Connector{Name: "maps", URL: "https://m.example", TokenEnv: "MAPS_KEY"}).token(ctx, nil); !errors.Is(err, ErrSetup) {
		t.Errorf("no vault, no variable: %v", err)
	}
}

// TestInv_SecretsStayOffCommandLines: storing a secret hands its value
// to the keychain command on standard input, never as an argument any
// process listing would show, on macOS and Linux alike; a value that
// isn't one line of printable ASCII without quotes or backslashes is
// refused before any command runs. (Roadmap 7.1.)
func TestInv_SecretsStayOffCommandLines(t *testing.T) {
	ctx := context.Background()
	for _, sys := range []string{"darwin", "linux"} {
		t.Run(sys, func(t *testing.T) {
			goos = sys
			t.Cleanup(func() { goos = runtime.GOOS })
			dir := t.TempDir()
			// Stand-ins for security and secret-tool: each logs its
			// arguments and standard input, and keeps one secret in a file.
			script := `#!/bin/sh
echo "ARGS $*" >> "$LOG"
case "$1" in
  -i) cat >> "$LOG"; cat "$LOG" | grep -o 'w "[^"]*"' | tail -1 | sed 's/w "//; s/"$//' | tr -d '\n' > "$STORE";;
  store) cat > "$STORE"; cat "$STORE" >> "$LOG";;
  find-generic-password|lookup) [ -n "$LOCKED" ] && exit 1; [ -s "$STORE" ] || exit $MISSING; cat "$STORE"; echo;;
  search) if [ -s "$STORE" ]; then echo "[/1] label = heedle"; fi;;
  delete-generic-password|clear) [ -n "$LOCKED" ] && exit 0; [ -s "$STORE" ] || exit $MISSING; rm "$STORE";;
  hang) sleep 5;;
esac
`
			name, missing := "secret-tool", "1"
			if sys == "darwin" {
				name, missing = "security", "44"
			}
			os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755)
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("LOG", filepath.Join(dir, "log"))
			t.Setenv("STORE", filepath.Join(dir, "store"))
			t.Setenv("MISSING", missing)
			var k Keychain
			if _, ok, err := k.Lookup(ctx, "MAPS_KEY"); ok || err != nil {
				t.Fatalf("empty keychain: %v %v", ok, err)
			}
			const value = "AIzaSy-very-secret_value.42"
			if err := k.Store(ctx, "MAPS_KEY", value); err != nil {
				t.Fatal(err)
			}
			if v, ok, err := k.Lookup(ctx, "MAPS_KEY"); !ok || err != nil || v != value {
				t.Fatalf("lookup = %q %v %v", v, ok, err)
			}
			if err := k.Delete(ctx, "MAPS_KEY"); err != nil {
				t.Fatal(err)
			}
			if err := k.Delete(ctx, "MAPS_KEY"); err != nil {
				t.Errorf("deleting what isn't there: %v", err)
			}
			log, _ := os.ReadFile(filepath.Join(dir, "log"))
			for _, line := range strings.Split(string(log), "\n") {
				if strings.HasPrefix(line, "ARGS ") && strings.Contains(line, value) {
					t.Errorf("the value was an argument: %s", line)
				}
			}
			if !strings.Contains(string(log), value) {
				t.Error("the value never reached the keychain command's input")
			}
			os.Remove(filepath.Join(dir, "log"))
			for _, bad := range []string{"", "has space", "quote\"d", `back\slash`, "two\nlines", "naïve", strings.Repeat("x", MaxSecret+1)} {
				if err := k.Store(ctx, "MAPS_KEY", bad); !errors.Is(err, ErrSecret) {
					t.Errorf("%q: %v", bad, err)
				}
			}
			for _, bad := range []string{"", "1KEY", "has-dash", "a b", "$(rm)"} {
				if err := k.Store(ctx, bad, "v"); !errors.Is(err, ErrSecret) {
					t.Errorf("name %q: %v", bad, err)
				}
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "log")); len(b) > 0 {
				t.Errorf("a refused secret ran a command: %s", b)
			}
			if sys == "linux" {
				// A locked keyring: secret-tool says nothing for a lookup and
				// "clears" without clearing (review 26).
				if err := k.Store(ctx, "MAPS_KEY", value); err != nil {
					t.Fatal(err)
				}
				t.Setenv("LOCKED", "1")
				if _, ok, err := k.Lookup(ctx, "MAPS_KEY"); ok || !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "locked") {
					t.Errorf("a locked keyring read as %v, %v", ok, err)
				}
				if err := k.Delete(ctx, "MAPS_KEY"); err == nil {
					t.Error("a delete a locked keyring ignored said it worked")
				}
			}
		})
	}
}

// TestKeychain_Real stores, reads, replaces and deletes secrets in this
// system's real keychain: the macOS login keychain or the Linux secret
// service. It runs only with PIRN_KEYCHAIN_TEST=1 (CI sets it, with a
// throwaway keychain), so it never touches yours, and then fails rather
// than skips.
func TestKeychain_Real(t *testing.T) {
	if os.Getenv("PIRN_KEYCHAIN_TEST") != "1" {
		t.Skip("set PIRN_KEYCHAIN_TEST=1 to use this system's keychain")
	}
	ctx := context.Background()
	var k Keychain
	name := "HEEDLE_TEST_KEY"
	t.Cleanup(func() { k.Delete(ctx, name) })
	// Every character a key or token uses: base64, base64url, JWT dots.
	for _, v := range []string{"tok-123", "sk-ant-api03-AbC_dEf-123", "eyJhbGciOi.eyJzdWIiOi.SflKxw+/=~!#$%&'()*,:;<>?@[]^`{|}"} {
		if err := k.Store(ctx, name, v); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := k.Lookup(ctx, name); got != v || !ok || err != nil {
			t.Fatalf("lookup = %q %v %v, want %q", got, ok, err, v)
		}
	}
	if err := k.Delete(ctx, name); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := k.Lookup(ctx, name); ok || err != nil {
		t.Fatalf("after delete: %v %v", ok, err)
	}
	if err := k.Delete(ctx, name); err != nil {
		t.Fatalf("deleting what isn't there: %v", err)
	}
}

// TestInv_KeychainNeverHangsOrOverflows: a keychain command that doesn't
// answer (a permission window nobody sees) is an error after
// KeychainTimeout; and the longest name and value make a macOS command
// line security -i reads whole (it reads 4096 bytes a line and runs the
// rest as another command). (Review 26.)
func TestInv_KeychainNeverHangsOrOverflows(t *testing.T) {
	line := securityLine(strings.Repeat("N", MaxSecretName), strings.Repeat("v", MaxSecret))
	if len(line) >= 4096 || CheckSecret(strings.Repeat("N", MaxSecretName+1), "v") == nil {
		t.Errorf("the longest command is %d bytes", len(line))
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secret-tool"), []byte("#!/bin/sh\nsleep 5\n"), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	goos, KeychainTimeout = "linux", 200*time.Millisecond
	t.Cleanup(func() { goos, KeychainTimeout = runtime.GOOS, 15*time.Second })
	start := time.Now()
	_, _, err := Keychain{}.Lookup(context.Background(), "MAPS_KEY")
	if !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "didn't answer") || time.Since(start) > 3*time.Second {
		t.Errorf("a keychain that doesn't answer: %v after %v", err, time.Since(start))
	}
}

// TestInv_ConnectorsReloadEachStep: a worker given LoadConnectors reads
// its connectors at the start of every step and write, so one added,
// changed or removed counts from the next step with no restart; a file
// it can't read fails the step as setup. (Roadmap 7.2.)
func TestInv_ConnectorsReloadEachStep(t *testing.T) {
	ctx := context.Background()
	calls := 0
	var now Connectors
	a := Agent{LoadConnectors: func() (Connectors, error) {
		calls++
		if now == nil {
			return nil, errors.New("connectors.json: unexpected end of JSON input")
		}
		return now, nil
	}}
	if _, err := a.RunWrite(ctx, Write{ID: "w1", Connector: "mail", Tool: "send", Input: []byte(`{}`)}); err == nil || !strings.Contains(err.Error(), "connectors") {
		t.Errorf("an unreadable file: %v", err)
	}
	if _, err := a.Run(ctx, Question{Instruction: "x"}); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "connectors") {
		t.Errorf("a step with an unreadable file: %v", err)
	}
	now = Connectors{{Name: "maps", URL: "https://maps.example/mcp"}}
	if _, err := a.RunWrite(ctx, Write{ID: "w1", Connector: "mail", Tool: "send", Input: []byte(`{}`)}); err == nil || !strings.Contains(err.Error(), "no write mail/send") {
		t.Errorf("a connector the file doesn't have: %v", err)
	}
	b, err := a.fresh()
	if err != nil || len(b.Connectors) != 1 || b.Connectors[0].Name != "maps" || calls != 4 {
		t.Errorf("fresh = %v, %v after %d reads", b.Connectors, err, calls)
	}
}
