package pirn_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"github.com/heedle-ai/bobbin"
	"github.com/heedle-ai/pirn"
	"github.com/heedle-ai/shuttle"
	"github.com/heedle-ai/shuttle/plan"
)

// TestInv_AgentStepsHeartbeatWhileTheyWait: an agent step heartbeats to
// Temporal while it waits on Claude, not only after each command, so a
// cancel reaches it; a skill's creation (Agent.Run from an activity)
// does too; and a step quiet for too long stops, so Temporal still
// notices a hung one (PIRN-502).
func TestInv_AgentStepsHeartbeatWhileTheyWait(t *testing.T) {
	var waiting atomic.Bool // Claude is answering
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		waiting.Store(true)
		time.Sleep(300 * time.Millisecond)
		waiting.Store(false)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, turn("end_turn", `{"type":"text","text":"done"}`))
	}))
	t.Cleanup(slow.Close)
	podman, _ := fakePodman(t, "ok")
	a := pirn.Agent{Claude: pirn.Claude{APIKey: "k", BaseURL: slow.URL}, Runner: bobbin.Runner{Podman: podman}, Image: zeros}
	// beats counts the heartbeats made while Claude was answering.
	beats := func(quiet time.Duration, name string, act any, in ...any) int32 {
		var n atomic.Int32
		defer pirn.SetStepBeatEvery(20*time.Millisecond, quiet, func() {
			if waiting.Load() {
				n.Add(1)
			}
		})()
		var s testsuite.WorkflowTestSuite
		env := s.NewTestActivityEnvironment()
		env.RegisterActivityWithOptions(act, activity.RegisterOptions{Name: name})
		if _, err := env.ExecuteActivity(name, in...); err != nil {
			t.Fatal(err)
		}
		return n.Load()
	}
	step := shuttle.ArrowAsActivity(pirn.AgentCommand(a))
	create := func(ctx context.Context) (string, error) {
		_, err := a.Run(ctx, pirn.Question{Instruction: "Write a weather skill."})
		return "", err
	}
	if n := beats(time.Minute, pirn.AgentActivityName, step, plan.Input{NodeID: "n0", Args: []string{"Sort it."}}); n < 1 {
		t.Errorf("an agent step waiting on Claude heartbeat %d times", n)
	}
	if n := beats(time.Minute, "Create", create); n < 1 {
		t.Errorf("creating a skill heartbeat %d times", n)
	}
	if n := beats(0, pirn.AgentActivityName, step, plan.Input{NodeID: "n0", Args: []string{"Sort it."}}); n != 0 {
		t.Errorf("a step quiet past its limit heartbeat %d times", n)
	}
}
