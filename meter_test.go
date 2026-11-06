package pirn_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/heedle-ai/bobbin"
	"github.com/heedle-ai/pirn"
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
	var none *pirn.Meter
	none.Add(pirn.Call{Model: "x"}) // a nil Meter counts nothing, and doesn't panic
	if none.Calls() != nil || pirn.MeterFrom(ctx) != nil {
		t.Error("no meter should count nothing")
	}
}
