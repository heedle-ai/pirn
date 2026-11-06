package pirn_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heedle-ai/pirn"
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
	if _, ok, err := s.Load(ctx, "forms.example.org"); ok || err == nil {
		t.Error("opened with another key")
	}
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
	if _, ok, err := s.Load(ctx, "evil.example.org"); ok || err == nil {
		t.Error("a session opened for another host")
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
	s.Load(ctx, "shop.example.org") // used: its thirty days start again
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
	// Forget all: every file and key.
	if err := s.ForgetAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("the folder is still there")
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

// TestInv_SessionLoadsOnlyForItsHost: a step starts signed in only on its
// own hosts that have a kept sign-in, matched exactly: never a parent
// domain's, a sub-domain's or a look-alike's; only that host's cookies and
// storage go in; the record names the hosts, never what they hold; and a
// step signed in as the person can't send without asking (roadmap 9.11).
func TestInv_SessionLoadsOnlyForItsHost(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newSessions(t)
	for _, h := range []string{"forms.example.org", "example.org", "sub.forms.example.org", "notforms.example.org"} {
		st := strings.ReplaceAll(state, "forms.example.org", h)
		if err := s.Save(ctx, h, []byte(st)); err != nil {
			t.Fatal(err)
		}
	}
	api := newActAPI(t, actTurn("end_turn", `{"type":"text","text":"Done.\nResult: finished"}`))
	a, b := newAct(t, api)
	a.Sessions = s
	res, err := a.Run(ctx, pirn.ActQuestion{Instruction: "x", URL: "https://forms.example.org/volunteer", Allow: []string{"cdn.example.org"}})
	if err != nil || strings.Join(res.SignedIn, " ") != "forms.example.org" {
		t.Fatalf("%v %v", err, res.SignedIn)
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
	if strings.Contains(string(out), "s3cr3t") || strings.Contains(string(sent), "s3cr3t") || !strings.Contains(string(out), `"signedIn":["forms.example.org"]`) {
		t.Errorf("record %s", out)
	}
	// A step with none of those hosts starts with none.
	a, b = newAct(t, newActAPI(t, actTurn("end_turn", `{"type":"text","text":"x"}`)))
	a.Sessions = s
	if res, err := a.Run(ctx, pirn.ActQuestion{Instruction: "x", URL: "https://shop.example.org/"}); err != nil || res.SignedIn != nil || b.last.storage != nil {
		t.Errorf("%v %v %s", err, res.SignedIn, b.last.storage)
	}
	// Signed in, it can't send without asking: nothing starts.
	a, b = newAct(t, newActAPI(t, actTurn("end_turn", `{"type":"text","text":"x"}`)))
	a.Sessions = s
	_, err = a.Run(ctx, pirn.ActQuestion{Instruction: "x", URL: "https://forms.example.org/", SendFreely: true})
	if !errors.Is(err, pirn.ErrInvalid) || !strings.Contains(err.Error(), "can't send without asking") || b.got.URL != "" {
		t.Errorf("%v, browser %v", err, b.got)
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
		driver(t, &a, func(w *pirn.Wheel) { w.HandBack(keep) })
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
	// No connector may name a session's key.
	err := pirn.Connectors{{Name: "x", URL: "https://x.example/mcp", TokenEnv: pirn.SessionKey("forms.example.org")}}.Check()
	if err == nil {
		t.Error("a connector named a session's key")
	}
}
