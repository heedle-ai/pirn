package pirn_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/heedle-ai/pirn"
)

// The script a run keeps, and its replay (roadmap 9.9).

var (
	signUp    = pirn.ActQuestion{Instruction: "Sign me up.", URL: "https://forms.example.org/volunteer", Node: "n1", Sources: []pirn.Source{{ID: "n0", Text: "Name: Ada"}}}
	navigate  = browserUse("t1", "browser_navigate", `{"url":"https://forms.example.org/volunteer"}`)
	look      = browserUse("t2", "browser_snapshot", `{}`)
	fillName  = browserUse("t3", "browser_fill_form", `{"fields":[{"name":"Name","type":"textbox","target":"e5","value":"Ada"}]}`)
	askToSend = use("c1", pirn.ConfirmTool, `{"summary":"Name Ada."}`)
	next      = browserUse("t4", "browser_click", `{"element":"Next","target":"e8"}`)
	finished  = actTurn("end_turn", `{"type":"text","text":"Entered Ada from n0 and pressed Next.\nResult: finished"}`)
)

// recorded is a first run, by Claude, of signUp: open, read, fill, ask,
// send. It returns the step's result.
func recorded(t *testing.T) pirn.ActResult {
	t.Helper()
	api := newActAPI(t, actTurn("tool_use", navigate), actTurn("tool_use", look), actTurn("tool_use", fillName), actTurn("tool_use", askToSend), actTurn("tool_use", next), finished)
	a, _ := newAct(t, api)
	a.Confirm = func(context.Context, pirn.ActPending) (bool, error) { return true, nil }
	res, err := a.Run(context.Background(), signUp)
	if err != nil || res.Ended != pirn.EndedFinished {
		t.Fatalf("%v %+v", err, res)
	}
	return roundTrip(t, res)
}

// roundTrip is res as a later run reads it from the record.
func roundTrip(t *testing.T, res pirn.ActResult) pirn.ActResult {
	t.Helper()
	got, ok := pirn.ParseAct(string(must(json.Marshal(res))))
	if !ok {
		t.Fatalf("not a strict act output: %s", must(json.Marshal(res)))
	}
	return got
}

// noClaude is a Messages API that fails the test if it is asked anything.
func noClaude(t *testing.T) *actAPI {
	api := newActAPI(t, finished)
	api.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		t.Error("Claude was asked during a replay of an unchanged page")
		w.WriteHeader(http.StatusInternalServerError)
	})
	return api
}

// from is an Act.Script that finds prev, as the run "r1" recorded it.
func from(prev pirn.ActResult, looked *int) func(context.Context, string) (pirn.ActScript, bool, error) {
	return func(_ context.Context, node string) (pirn.ActScript, bool, error) {
		if looked != nil {
			*looked++
		}
		return pirn.ActScript{Run: "r1", Record: strings.Repeat("c", 40), Result: prev}, node == "n1", nil
	}
}

// TestInv_ActKeepsAScript: an act step's record keeps what it did as a
// script: each action that ran (not reading the page), with each element
// it targeted as a Playwright locator by role and name (never the page's
// ref, which changes between runs), the page's outline before it (the
// text, and its SHA-256), the confirmation with its summary, and the
// question it is good for. (Roadmap 9.9.)
func TestInv_ActKeepsAScript(t *testing.T) {
	res := recorded(t)
	var tools []string
	for _, st := range res.Script {
		tools = append(tools, st.Tool)
		if len(st.OutlineSHA) != 64 || st.Replayed || strings.Contains(st.Input, `"e5"`) || strings.Contains(st.Input, `"e8"`) || strings.Contains(st.Outline, "ref=") {
			t.Errorf("step %+v", st)
		}
	}
	if strings.Join(tools, " ") != "browser_navigate browser_fill_form confirm browser_click" {
		t.Fatalf("script: %v", tools)
	}
	fill, click := res.Script[1], res.Script[3]
	if fill.Targets[0] != (pirn.Locator{Role: "textbox", Name: "Name"}) || !strings.Contains(fill.Input, `"target":"@0"`) || !strings.Contains(fill.Input, `"value":"Ada"`) ||
		click.Targets[0] != (pirn.Locator{Role: "button", Name: "Next"}) || fill.Targets[0].String() != `getByRole("textbox", { name: "Name", exact: true })` {
		t.Errorf("locators: %+v %+v", fill, click)
	}
	if !strings.Contains(fill.Outline, `- textbox "Name"`) || !strings.Contains(res.Script[2].Input, "Name Ada.") || res.ScriptInputs != pirn.InputsSHA(signUp) || res.Replay != nil {
		t.Errorf("record: %+v", res)
	}
	// A step whose element can't be named (a ref no snapshot gave) keeps
	// no script: one with a gap would replay as if it were all of it.
	api := newActAPI(t, actTurn("tool_use", navigate), actTurn("tool_use", browserUse("t9", "browser_click", `{"element":"?","target":"e77"}`)), actTurn("tool_use", fillName), finished)
	a, _ := newAct(t, api)
	got, err := a.Run(context.Background(), signUp)
	if err != nil || len(got.Script) != 0 {
		t.Errorf("%v %+v", err, got.Script)
	}
}

// TestInv_ReplayMakesNoClaudeCalls: the next run of the same program, on
// an unchanged page, replays the script without asking Claude at all (the
// stand-in Claude fails the test if asked): every action through the
// browser, each element found again by role and name on the page as it
// is now, the send asked about as before and sent through the hold; the
// record says which run's script it replayed, how far, and has no
// tokens; and its own script is the same. A program that says
// :replay "off" doesn't look. (Roadmap 9.9.)
func TestInv_ReplayMakesNoClaudeCalls(t *testing.T) {
	prev := recorded(t)
	a, b := newAct(t, noClaude(t))
	looked, asked := 0, 0
	a.Script = from(prev, &looked)
	a.Confirm = func(_ context.Context, p pirn.ActPending) (bool, error) {
		asked++
		if p.Summary != "Name Ada." {
			t.Errorf("asked about %q", p.Summary)
		}
		return true, nil
	}
	res, err := a.Run(context.Background(), signUp)
	if err != nil {
		t.Fatal(err)
	}
	res = roundTrip(t, res)
	if res.Ended != pirn.EndedFinished || res.Turns != 0 || res.InputTokens != 0 || res.OutputTokens != 0 || asked != 1 || looked != 1 ||
		res.Replay == nil || *res.Replay != (pirn.ActReplay{From: "r1", Record: strings.Repeat("c", 40), Steps: 4, Replayed: 4}) || !strings.Contains(res.Text, "without Claude") {
		t.Fatalf("result: %+v %+v", res, res.Replay)
	}
	for _, c := range res.Calls {
		if !c.Replayed || c.IsError {
			t.Errorf("call %+v", c)
		}
	}
	// The click that sends comes straight after reading the page it was
	// asked about on (and checking it asks for no card).
	if c := codes(t, b); !strings.Contains(c, "browser_snapshot browser_evaluate release browser_click close") || strings.Count(c, "release") != 1 || strings.Count(c, "browser_fill_form") != 1 {
		t.Errorf("the browser ran: %s", c)
	}
	for i := range res.Script {
		if !res.Script[i].Replayed {
			t.Errorf("step %d not marked replayed", i)
		}
		res.Script[i].Replayed = false
	}
	if !reflect.DeepEqual(res.Script, prev.Script) {
		t.Errorf("the replay's script differs:\n%+v\n%+v", res.Script, prev.Script)
	}
	// :replay "off": Claude does it all, and no script is looked for.
	if o, ok := pirn.ActUses([]string{"x", "https://forms.example.org/", pirn.Replay, "off"}); !ok || o != nil {
		t.Errorf("%v %v", o, ok)
	}
	for _, bad := range [][]string{{pirn.Replay}, {pirn.Replay, "never"}, {pirn.Replay, "off", pirn.Replay, "on"}} {
		if _, ok := pirn.ActHosts(append([]string{"x", "https://forms.example.org/"}, bad...)); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	api := newActAPI(t, finished)
	a, _ = newAct(t, api)
	looked = 0
	a.Script = from(prev, &looked)
	q := signUp
	q.NoReplay = true
	if res, err := a.Run(context.Background(), q); err != nil || looked != 0 || res.Replay != nil || len(api.reqs) != 1 {
		t.Errorf("replay off: %v, looked %d, %+v, asked Claude %d times", err, looked, res.Replay, len(api.reqs))
	}
}

// TestInv_ReplayHandsOverWhereThePageDiffers: a renamed field makes the
// page differ from the recorded one at the action that fills it: the
// replay does what came before (opening the page), then hands the rest
// to Claude, telling it where and why, what was done and the page now;
// the record says where (that action, naming the line that differs). A
// page that changes by itself never replays past its first action, and
// neither does one asking for a card, or a browser that can't say.
// (Roadmap 9.9.)
func TestInv_ReplayHandsOverWhereThePageDiffers(t *testing.T) {
	prev := recorded(t)
	t.Run("renamed", func(t *testing.T) {
		t.Setenv("FAKE_RENAME", "1")
		api := newActAPI(t, actTurn("tool_use", look), finished)
		a, b := newAct(t, api)
		a.Script = from(prev, nil)
		res, err := a.Run(context.Background(), signUp)
		if err != nil {
			t.Fatal(err)
		}
		rp := res.Replay
		if rp == nil || rp.Replayed != 1 || !rp.HandedOver || rp.At != 2 || !strings.Contains(rp.Why, `it has no getByRole("textbox", { name: "Name", exact: true })`) || !strings.Contains(rp.Why, `textbox \"Full name\"`) {
			t.Fatalf("replay: %+v", rp)
		}
		if strings.Count(codes(t, b), "browser_fill_form") != 0 || !res.Calls[0].Replayed || res.Calls[1].Replayed || res.Turns != 2 {
			t.Errorf("calls %+v, browser %s", res.Calls, codes(t, b))
		}
		msg, _ := json.Marshal(api.reqs[0]["messages"])
		for _, w := range []string{"It stopped before action 2 of 4", "Full name", "1. browser_navigate", "The page now:"} {
			if !strings.Contains(string(msg), w) {
				t.Errorf("Claude wasn't told %q: %s", w, msg)
			}
		}
	})
	for name, env := range map[string]string{"changing": "FAKE_PAGE_CHANGES", "payment": "FAKE_PAYMENT"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(env, "1")
			api := newActAPI(t, finished)
			a, b := newAct(t, api)
			a.Script = from(prev, nil)
			res, err := a.Run(context.Background(), signUp)
			want := map[string]string{"changing": "differs", "payment": "card"}[name]
			if err != nil || res.Replay == nil || res.Replay.Replayed != 0 || res.Replay.At != 1 || !strings.Contains(res.Replay.Why, want) || strings.Contains(codes(t, b), "browser_navigate") || len(api.reqs) != 1 {
				t.Errorf("%v %+v, browser %s", err, res.Replay, codes(t, b))
			}
		})
	}
}

// TestInv_ReplayNeverSendsWithoutAYes: a replayed send asks the person,
// as Claude's does: a no, or no one to ask, ends the step there, with the
// way for the send never opened and nothing after it done. (Roadmap 9.9.)
func TestInv_ReplayNeverSendsWithoutAYes(t *testing.T) {
	prev := recorded(t)
	for name, confirm := range map[string]func(context.Context, pirn.ActPending) (bool, error){
		"no":     func(context.Context, pirn.ActPending) (bool, error) { return false, nil },
		"no one": nil,
		"failed": func(context.Context, pirn.ActPending) (bool, error) { return false, errors.New("unreadable") },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := newAct(t, noClaude(t))
			a.Script = from(prev, nil)
			a.Confirm = confirm
			res, err := a.Run(context.Background(), signUp)
			if err != nil || res.Ended != pirn.EndedCancelled || res.Replay.Replayed != 2 {
				t.Fatalf("%v %+v %+v", err, res, res.Replay)
			}
			if c := codes(t, b); strings.Contains(c, "release") || strings.Contains(c, "browser_click") {
				t.Errorf("sent: %s", c)
			}
		})
	}
}

// TestInv_ReplayOnlyForItsQuestion: a script replays only for the same
// question: a step whose inputs changed (an earlier step answered
// differently), one from a run that didn't finish, or with no script,
// asks Claude from the start, and the record says why. One the lookup
// can't read is said too. (Roadmap 9.9.)
func TestInv_ReplayOnlyForItsQuestion(t *testing.T) {
	prev := recorded(t)
	stopped := prev
	stopped.Ended = pirn.EndedStopped
	empty := prev
	empty.Script = nil
	changed := signUp
	changed.Sources = []pirn.Source{{ID: "n0", Text: "Name: Grace"}}
	for name, c := range map[string]struct {
		prev pirn.ActResult
		q    pirn.ActQuestion
		why  string
	}{
		"inputs":  {prev, changed, "inputs differ"},
		"stopped": {stopped, signUp, "didn't finish"},
		"empty":   {empty, signUp, "no script"},
	} {
		t.Run(name, func(t *testing.T) {
			api := newActAPI(t, finished)
			a, b := newAct(t, api)
			a.Script = from(c.prev, nil)
			res, err := a.Run(context.Background(), c.q)
			if err != nil || res.Replay == nil || res.Replay.Replayed != 0 || !strings.Contains(res.Replay.Why, c.why) || len(api.reqs) != 1 || strings.Contains(codes(t, b), "browser_navigate") {
				t.Errorf("%v %+v", err, res.Replay)
			}
		})
	}
	api := newActAPI(t, finished)
	a, _ := newAct(t, api)
	a.Script = func(context.Context, string) (pirn.ActScript, bool, error) {
		return pirn.ActScript{}, false, errors.New("store gone")
	}
	if res, err := a.Run(context.Background(), signUp); err != nil || res.Replay == nil || !strings.Contains(res.Replay.Why, "store gone") || len(api.reqs) != 1 {
		t.Errorf("%v %+v", err, res.Replay)
	}
}

// TestInv_ScriptsAreStrict: a record whose script names a tool a replay
// may not run (a script, an upload), an element it doesn't list, or no
// outline hash, isn't an act record at all, so it is never replayed.
// (Roadmap 9.9.)
func TestInv_ScriptsAreStrict(t *testing.T) {
	prev := recorded(t)
	for name, change := range map[string]func(*pirn.ActResult){
		"evaluate":    func(r *pirn.ActResult) { r.Script[0].Tool = "browser_evaluate" },
		"upload":      func(r *pirn.ActResult) { r.Script[0].Tool = "browser_file_upload" },
		"snapshot":    func(r *pirn.ActResult) { r.Script[0].Tool = "browser_snapshot" },
		"target":      func(r *pirn.ActResult) { r.Script[1].Input = strings.Replace(r.Script[1].Input, "@0", "@3", 1) },
		"ref":         func(r *pirn.ActResult) { r.Script[1].Input = strings.Replace(r.Script[1].Input, "@0", "e5", 1) },
		"no role":     func(r *pirn.ActResult) { r.Script[1].Targets[0].Role = "" },
		"hash":        func(r *pirn.ActResult) { r.Script[2].OutlineSHA = "x" },
		"input":       func(r *pirn.ActResult) { r.Script[2].Input = "[1]" },
		"inputs hash": func(r *pirn.ActResult) { r.ScriptInputs = "nope" },
		"replayed":    func(r *pirn.ActResult) { r.Replay = &pirn.ActReplay{Steps: 1, Replayed: 2} },
	} {
		r := prev
		r.Script = append([]pirn.ScriptStep(nil), prev.Script...)
		for i := range r.Script {
			r.Script[i].Targets = append([]pirn.Locator(nil), prev.Script[i].Targets...)
		}
		change(&r)
		if _, ok := pirn.ParseAct(string(must(json.Marshal(r)))); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestLaw_ReplayReproducesItsScript is Soundness law 22: on an unchanged
// page, replay is a fixed point of recording. For random act steps
// (fills, clicks, key presses, pages opened, sends asked about), a second
// run replaying the first's script does exactly its actions, asks Claude
// nothing, and records the same script: script(replay(s)) = s.
func TestLaw_ReplayReproducesItsScript(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	moves := []string{
		navigate,
		fillName,
		browserUse("k", "browser_type", `{"element":"Name","target":"e5","text":"Ada"}`),
		browserUse("p", "browser_press_key", `{"key":"ArrowDown"}`),
		browserUse("s", "browser_select_option", `{"element":"Name","target":"e5","values":["Ada"]}`),
		browserUse("h", "browser_hover", `{"element":"Next","target":"e8"}`),
		next,
	}
	for i := 0; i < 25; i++ {
		var turns []string
		for n := 1 + r.Intn(6); n > 0; n-- {
			if r.Intn(4) == 0 {
				turns = append(turns, actTurn("tool_use", askToSend), actTurn("tool_use", next))
				continue
			}
			turns = append(turns, actTurn("tool_use", moves[r.Intn(len(moves))]))
		}
		turns = append(turns, finished)
		yes := func(context.Context, pirn.ActPending) (bool, error) { return true, nil }
		a, _ := newAct(t, newActAPI(t, turns...))
		a.Confirm = yes
		first, err := a.Run(context.Background(), signUp)
		if err != nil {
			t.Fatal(err)
		}
		first = roundTrip(t, first)
		a, b := newAct(t, noClaude(t))
		a.Confirm, a.Script = yes, from(first, nil)
		second, err := a.Run(context.Background(), signUp)
		if err != nil {
			t.Fatal(err)
		}
		second = roundTrip(t, second)
		for j := range second.Script {
			second.Script[j].Replayed = false
		}
		if !reflect.DeepEqual(first.Script, second.Script) || second.Replay.Replayed != len(first.Script) || second.Ended != pirn.EndedFinished {
			t.Fatalf("program %d: script %v\nreplayed as %v (%+v)\nbrowser: %s", i, first.Script, second.Script, second.Replay, codes(t, b))
		}
	}
}
