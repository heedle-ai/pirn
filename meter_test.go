package pirn_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/heedle-ai/bobbin"
	"github.com/heedle-ai/pirn"
	"github.com/heedle-ai/shuttle/plan"
)

// TestInv_EveryCallIsMetered: every request Claude answers is noted on
// the context's Meter the moment it answers, an ask's and each agent
// turn's alike, even when the step then fails: here the second turn is
// refused, and the first, which Claude answered, is still counted; a
// refused request is not (PIRN-320, roadmap 10.4.1).
func TestInv_EveryCallIsMetered(t *testing.T) {
	ctx := context.Background()
	ask := newScript(t, turn("end_turn", `{"type":"text","text":"hi"}`))
	m := &pirn.Meter{}
	if _, err := (pirn.Claude{APIKey: "k", BaseURL: ask.srv.URL}).Ask(pirn.WithMeter(ctx, m), pirn.Question{Instruction: "Say hi."}); err != nil {
		t.Fatal(err)
	}
	if got := m.Calls(); !reflect.DeepEqual(got, []pirn.Call{{Model: "claude-sonnet-5-5", Input: 10, Output: 3}}) {
		t.Errorf("an ask's calls: %+v", got)
	}

	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if n.Add(1) == 1 {
			io.WriteString(w, turn("tool_use", toolUse("t1", "ls")))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`)
	}))
	t.Cleanup(srv.Close)
	podman, _ := fakePodman(t, "ok")
	a := pirn.Agent{Claude: pirn.Claude{APIKey: "k", BaseURL: srv.URL}, Runner: bobbin.Runner{Podman: podman}, Image: zeros}
	m = &pirn.Meter{}
	if _, err := a.Run(pirn.WithMeter(ctx, m), pirn.Question{Instruction: "List."}); err == nil {
		t.Fatal("the agent step should fail on the refused turn")
	}
	if got := m.Calls(); !reflect.DeepEqual(got, []pirn.Call{{Model: "claude-sonnet-5-5", Input: 10, Output: 3}}) {
		t.Errorf("a failed agent step's calls: %+v", got)
	}
	// An act step's turns too (review 56).
	act, _ := newAct(t, newActAPI(t, actTurn("end_turn", `{"type":"text","text":"Nothing to do.\nResult: finished"}`)))
	m = &pirn.Meter{}
	if _, err := pirn.ActCommand(act)(pirn.WithMeter(ctx, m), plan.Input{NodeID: "n3", Args: []string{"Sign up.", "https://forms.example.org/"}}); err != nil {
		t.Fatal(err)
	}
	if got := m.Calls(); len(got) != 1 || got[0].Input == 0 {
		t.Errorf("an act step's calls: %+v", got)
	}

	// Every request pirn makes is metered where Claude answers it: each
	// call of the SDK's Messages.New is followed by MeterFrom(ctx).Add.
	files, _ := filepath.Glob("*.go")
	sites := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if !strings.Contains(l, "Messages.New(") {
				continue
			}
			sites++
			if !strings.Contains(strings.Join(lines[i:min(i+20, len(lines))], "\n"), "MeterFrom(ctx).Add(") {
				t.Errorf("%s:%d: a request to Claude that isn't metered", f, i+1)
			}
		}
	}
	if sites < 2 {
		t.Errorf("found %d requests to Claude; pirn makes at least an ask's and an agent turn's", sites)
	}
	var none *pirn.Meter
	none.Add(pirn.Call{Model: "x"}) // a nil Meter counts nothing, and doesn't panic
	if none.Calls() != nil || pirn.MeterFrom(ctx) != nil {
		t.Error("no meter should count nothing")
	}
}
