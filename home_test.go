package pirn_test

import (
	"context"
	"reflect"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/heedle-ai/bobbin"
	"github.com/heedle-ai/pirn"
	"github.com/heedle-ai/reed"
	"github.com/heedle-ai/shuttle"
	"github.com/heedle-ai/shuttle/plan"
)

// TestInv_PirnContainersSayTheirHome: under a worker whose activities
// start from its home (bobbin.WithHome on the worker's context), every
// container pirn starts says that home: an agent step's workspace, a
// skill's creation, and an act step's browser, whose visit gets it from
// bobbin's context even when reed's says none; and bobbin and reed agree
// on the home label's name and on which homes a label may hold
// (PIRN-520, roadmap 9.2.1).
func TestInv_PirnContainersSayTheirHome(t *testing.T) {
	const home = "0123456789ab"
	var s testsuite.WorkflowTestSuite
	opts := worker.Options{BackgroundActivityContext: bobbin.WithHome(context.Background(), home)}

	// An agent step, in a program.
	env := s.NewTestActivityEnvironment()
	env.SetWorkerOptions(opts)
	wf, run := execution(t, env)
	api := newScript(t, turn("end_turn", `{"type":"text","text":"done"}`))
	podman, log := fakePodman(t, "ok")
	a := pirn.Agent{Claude: pirn.Claude{APIKey: "k", BaseURL: api.srv.URL}, Runner: bobbin.Runner{Podman: podman}, Image: zeros}
	env.RegisterActivityWithOptions(shuttle.ArrowAsActivity(pirn.AgentCommand(a)), activity.RegisterOptions{Name: pirn.AgentActivityName})
	if _, err := env.ExecuteActivity(pirn.AgentActivityName, plan.Input{NodeID: "n2", Args: []string{"Sort it."}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"heedle.kind=agent", "heedle.workflow=" + wf, "heedle.run=" + run, "heedle.step=n2", "heedle.home=" + home}
	if got := runLabels(t, log); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("agent step: %q, want %q", got, want)
	}

	// A skill's creation: Agent.Run straight from an activity.
	env = s.NewTestActivityEnvironment()
	env.SetWorkerOptions(opts)
	wf, run = execution(t, env)
	podman, log = fakePodman(t, "ok")
	a.Runner = bobbin.Runner{Podman: podman}
	env.RegisterActivityWithOptions(func(ctx context.Context) (string, error) {
		_, err := a.Run(ctx, pirn.Question{Instruction: "Write a weather skill."})
		return "", err
	}, activity.RegisterOptions{Name: "Create"})
	if _, err := env.ExecuteActivity("Create"); err != nil {
		t.Fatal(err)
	}
	want = []string{"heedle.kind=agent", "heedle.workflow=" + wf, "heedle.run=" + run, "heedle.home=" + home}
	if got := runLabels(t, log); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("creating a skill: %q, want %q", got, want)
	}

	// An act step's browser.
	env = s.NewTestActivityEnvironment()
	env.SetWorkerOptions(opts)
	wf, run = execution(t, env)
	act, b := newAct(t, newActAPI(t, actTurn("end_turn", `{"type":"text","text":"Nothing to do.\nResult: finished"}`)))
	env.RegisterActivityWithOptions(shuttle.ArrowAsActivity(pirn.ActCommand(act)), activity.RegisterOptions{Name: pirn.ActActivityName})
	if _, err := env.ExecuteActivity(pirn.ActActivityName, plan.Input{NodeID: "n3", Args: []string{"Sign up.", "https://forms.example.org/"}}); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{reed.LabelWorkflow: wf, reed.LabelRun: run, reed.LabelStep: "n3", reed.LabelHome: home}; !reflect.DeepEqual(b.got.Labels, want) {
		t.Errorf("act visit labels %v, want %v", b.got.Labels, want)
	}

	// bobbin and reed agree on the home.
	if bobbin.LabelHome != reed.LabelHome {
		t.Fatal("bobbin and reed name the home label differently")
	}
	env = s.NewTestActivityEnvironment()
	env.RegisterActivityWithOptions(func(ctx context.Context, h string) (bool, error) {
		ctx = reed.WithHome(bobbin.WithHome(ctx, h), h)
		return reflect.DeepEqual(bobbin.StepLabels(ctx, "n0").Map(), reed.StepLabels(ctx, "n0")), nil
	}, activity.RegisterOptions{Name: "Agree"})
	for _, h := range []string{home, "", "ABCDEF012345", "0123456789a", "0123456789abc", "--privileged", "/home/me"} {
		v, err := env.ExecuteActivity("Agree", h)
		if err != nil {
			t.Fatal(err)
		}
		var same bool
		if v.Get(&same); !same {
			t.Errorf("for home %q, bobbin and reed label differently", h)
		}
	}
}
