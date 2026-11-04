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
	// Every heedle key, only with a throwaway keychain (CI's).
	if os.Getenv("PIRN_KEYCHAIN_TEST_DELETE_ALL") == "1" {
		k.Store(ctx, name, "a1")
		k.Store(ctx, name+"_2", "a2")
		if n, err := k.DeleteAll(ctx); err != nil || (goos == "darwin" && n != 2) {
			t.Fatalf("DeleteAll: %d %v", n, err)
		}
		for _, n := range []string{name, name + "_2"} {
			if _, ok, err := k.Lookup(ctx, n); ok || err != nil {
				t.Errorf("%s after DeleteAll: %v %v", n, ok, err)
			}
		}
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
	// Retryable, not a setup error: the file may be mid-save (review 27).
	if _, err := a.Run(ctx, Question{Instruction: "x"}); err == nil || errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "connectors") {
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
	// A connector added to the file counts at once: its write passes the
	// worker's checks and goes on to its server (review 27).
	now = append(now, Connector{Name: "mail", URL: "http://127.0.0.1:1/mcp", Writes: []string{"send"}})
	if _, err := a.RunWrite(ctx, Write{ID: "w1", Connector: "mail", Tool: "send", Input: []byte(`{}`)}); err == nil || strings.Contains(err.Error(), "no write") {
		t.Errorf("a connector added later: %v", err)
	}
}

// TestInv_WorkersKeepTheLastGood: LastGood gives what its load last gave
// when it fails, so a worker keeps its connectors while their file is
// being saved and its key while the keychain is locked for a moment; with
// nothing before, the failure is load's. (Review 27.)
func TestInv_WorkersKeepTheLastGood(t *testing.T) {
	v, fail := "first", true
	f := LastGood(func() (string, error) {
		if fail {
			return "", errors.New("mid-save")
		}
		return v, nil
	})
	if _, err := f(); err == nil {
		t.Error("nothing to fall back on, yet no error")
	}
	fail = false
	if got, err := f(); got != "first" || err != nil {
		t.Errorf("good = %q, %v", got, err)
	}
	fail, v = true, "second"
	if got, err := f(); got != "first" || err != nil {
		t.Errorf("a failure after a good read = %q, %v", got, err)
	}
	fail = false
	if got, _ := f(); got != "second" {
		t.Errorf("the next good read = %q", got)
	}
}

// TestInv_AgentStepsReadTheKeyToo: an agent step gets its key from
// KeyFrom (a keychain-only worker), past the key check; a key it can't
// read now is retryable, not a setup error. (Review 27: reverting
// Agent.Run's check went unnoticed.)
func TestInv_AgentStepsReadTheKeyToo(t *testing.T) {
	ctx := context.Background()
	a := Agent{Claude: Claude{KeyFrom: func(context.Context) (string, error) { return "sk-from-keychain", nil }}}
	if _, err := a.Run(ctx, Question{Instruction: "x"}); err == nil || strings.Contains(err.Error(), "API key") || !strings.Contains(err.Error(), "agent image") {
		t.Errorf("a key from the keychain: %v", err)
	}
	a.Claude.KeyFrom = func(context.Context) (string, error) { return "", errors.New("locked") }
	if _, err := a.Run(ctx, Question{Instruction: "x"}); err == nil || errors.Is(err, ErrSetup) {
		t.Errorf("a key that can't be read now: %v", err)
	}
	a.Claude.KeyFrom = func(context.Context) (string, error) { return "", nil }
	if _, err := a.Run(ctx, Question{Instruction: "x"}); !errors.Is(err, ErrSetup) {
		t.Errorf("no key at all: %v", err)
	}
}

// TestInv_DeleteAllTakesOnlyHeedles: DeleteAll removes every item filed
// under heedle's service and asks for nothing else: on macOS one at a
// time until security says none is left (and counts them), on Linux in
// one clear by the service, then looks again, so a locked keyring that
// kept them is an error, never "done". (Roadmap 7.1.)
func TestInv_DeleteAllTakesOnlyHeedles(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { goos = runtime.GOOS })
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LOG", filepath.Join(dir, "log"))
	t.Setenv("LEFT", filepath.Join(dir, "left"))

	goos = "darwin"
	os.WriteFile(filepath.Join(dir, "security"), []byte(`#!/bin/sh
echo "$*" >> "$LOG"
n=$(cat "$LEFT"); [ "$n" -gt 0 ] || exit 44
echo $((n-1)) > "$LEFT"
`), 0o755)
	os.WriteFile(filepath.Join(dir, "left"), []byte("3\n"), 0o644)
	if n, err := (Keychain{}).DeleteAll(ctx); n != 3 || err != nil {
		t.Errorf("macOS: %d %v", n, err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "log"))
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		if line != "delete-generic-password -s heedle" {
			t.Errorf("macOS asked: %q", line)
		}
	}

	goos = "linux"
	os.Remove(filepath.Join(dir, "log"))
	os.WriteFile(filepath.Join(dir, "secret-tool"), []byte(`#!/bin/sh
echo "$*" >> "$LOG"
case "$1" in
  clear) [ -n "$LOCKED" ] || : > "$LEFT";;
  search) [ -s "$LEFT" ] && echo "[/1] label = heedle X";;
esac
exit 0
`), 0o755)
	os.WriteFile(filepath.Join(dir, "left"), []byte("2\n"), 0o644)
	if n, err := (Keychain{}).DeleteAll(ctx); n != -1 || err != nil {
		t.Errorf("Linux: %d %v", n, err)
	}
	if log, _ := os.ReadFile(filepath.Join(dir, "log")); string(log) != "clear service heedle\nsearch service heedle\n" {
		t.Errorf("Linux asked: %q", log)
	}
	t.Setenv("LOCKED", "1")
	os.WriteFile(filepath.Join(dir, "left"), []byte("2\n"), 0o644)
	if _, err := (Keychain{}).DeleteAll(ctx); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("a locked keyring: %v", err)
	}
}

// TestInv_ReadyAgreesWithRuns: a connector is called ready exactly when a
// step starting now would get it: the worker grants what's asked
// (grantFor) and every key it needs can be read (secretFor); otherwise
// Ready says why, naming the key. (Roadmap 8.2.)
func TestInv_ReadyAgreesWithRuns(t *testing.T) {
	ctx := context.Background()
	env := map[string]string{"ENV_KEY": "e"}
	getenv = func(k string) string { return env[k] }
	t.Cleanup(func() { getenv = os.Getenv })
	v := fakeVault{"MAPS_KEY": "m", "SEARCH_KEY": "s"}
	cs := Connectors{
		{Name: "maps", URL: "https://maps.example/mcp", TokenEnv: "MAPS_KEY", Tools: []string{"search", "directions"}},
		{Name: "search", Image: "img", Command: []string{"s"}, Secrets: map[string]string{"API_KEY": "SEARCH_KEY", "OTHER": "ENV_KEY"}},
		{Name: "mail", URL: "https://mail.example/mcp", TokenEnv: "MAIL_KEY"},
		{Name: "locked", URL: "https://l.example/mcp", TokenEnv: "BROKEN"},
		{Name: "open", URL: "https://open.example/mcp"},
	}
	for _, c := range []struct {
		grant, problem, key string
	}{
		{"maps", "tools", ""}, {"maps/search", "", ""}, {"maps/search maps/save", "tools", ""},
		{"search", "", ""}, {"mail", "key", "MAIL_KEY"}, {"locked", "unreadable", "BROKEN"},
		{"open", "", ""}, {"calendar", "absent", ""},
	} {
		g, err := ParseGrant(strings.Fields(c.grant))
		if err != nil {
			t.Fatal(err)
		}
		needs := cs.Ready(ctx, g, v)
		if len(needs) != 1 || needs[0].Problem != c.problem || needs[0].Key != c.key {
			t.Errorf("%s: %+v, want problem %q key %q", c.grant, needs, c.problem, c.key)
			continue
		}
		// The run's own view: the grant, then each key.
		runOK := true
		if _, err := cs.grantFor(g); err != nil {
			runOK = false
		} else {
			conn, _ := cs.Find(g.Connectors()[0])
			if _, err := conn.token(ctx, v); err != nil {
				runOK = false
			}
			if _, err := conn.secrets(ctx, v); err != nil {
				runOK = false
			}
		}
		if runOK != (needs[0].Problem == "") {
			t.Errorf("%s: Ready says %q, but a run would %s", c.grant, needs[0].Problem, map[bool]string{true: "start", false: "fail"}[runOK])
		}
	}
	// Review 33: what a step could change, and whether it waits for you.
	writer := Connectors{{Name: "mail", URL: "https://mail.example/mcp", Writes: []string{"send", "delete"}, Approve: true}, {Name: "slack", URL: "https://s.example/mcp", Writes: []string{"post"}}}
	g, _ := ParseGrant([]string{"mail/send", "mail/read", "slack"})
	if n := writer.Ready(ctx, g, v); n[0].Connector != "mail" || strings.Join(n[0].Writes, ",") != "send" || !n[0].Approve || strings.Join(n[1].Writes, ",") != "post" || n[1].Approve {
		t.Errorf("writes: %+v", n)
	}
	if n := cs.Ready(ctx, Grant{}, v); len(n) != 0 {
		t.Errorf("no grant, no needs: %+v", n)
	}
	if n := (Connectors{{Name: "maps", URL: "https://m", TokenEnv: "MAPS_KEY"}}).Ready(ctx, Grant{"maps": {All: true}}, nil); n[0].Problem != "key" {
		t.Errorf("no vault: %+v", n)
	}
}
