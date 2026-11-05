package pirn_test

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"github.com/heedle-ai/bobbin"
	"github.com/heedle-ai/pirn"
	"github.com/heedle-ai/reed"
	"github.com/heedle-ai/shuttle"
	"github.com/heedle-ai/shuttle/plan"
)

// runLabels are the heedle labels of each podman run in a fake podman's
// log (one call a line), in order.
func runLabels(t *testing.T, log string) [][]string {
	t.Helper()
	b, _ := os.ReadFile(log)
	label := regexp.MustCompile(`--label (heedle\.[a-z]+=\S+)`)
	var out [][]string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "run ") {
			var ls []string
			for _, m := range label.FindAllStringSubmatch(l, -1) {
				ls = append(ls, m[1])
			}
			out = append(out, ls)
		}
	}
	return out
}

// execution is the test environment's workflow and run, as its
// activities see them.
func execution(t *testing.T, env *testsuite.TestActivityEnvironment) (string, string) {
	t.Helper()
	env.RegisterActivityWithOptions(func(ctx context.Context) ([]string, error) {
		e := activity.GetInfo(ctx).WorkflowExecution
		return []string{e.ID, e.RunID}, nil
	}, activity.RegisterOptions{Name: "Execution"})
	v, err := env.ExecuteActivity("Execution")
	if err != nil {
		t.Fatal(err)
	}
	var e []string
	v.Get(&e)
	if e[0] == "" || e[1] == "" {
		t.Fatal("the test environment has no execution")
	}
	return e[0], e[1]
}

// TestInv_EveryContainerPirnStartsIsLabelled: an agent step's workspace
// is labelled agent, a skill step's skill, a connector's server (in a step
// or for a write) connector and an act step's browser act, each with the
// step's run labels from Temporal's activity info: the workflow that
// scheduled it, that workflow's run and, inside a plan, the step's node
// ID (PIRN-500).
func TestInv_EveryContainerPirnStartsIsLabelled(t *testing.T) {
	var s testsuite.WorkflowTestSuite

	// An agent step, in a program.
	env := s.NewTestActivityEnvironment()
	wf, run := execution(t, env)
	api := newScript(t, turn("end_turn", `{"type":"text","text":"done"}`))
	podman, log := fakePodman(t, "ok")
	a := pirn.Agent{Claude: pirn.Claude{APIKey: "k", BaseURL: api.srv.URL}, Runner: bobbin.Runner{Podman: podman}, Image: zeros}
	env.RegisterActivityWithOptions(shuttle.ArrowAsActivity(pirn.AgentCommand(a)), activity.RegisterOptions{Name: pirn.AgentActivityName})
	if _, err := env.ExecuteActivity(pirn.AgentActivityName, plan.Input{NodeID: "n2", Args: []string{"Sort it."}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"heedle.kind=agent", "heedle.workflow=" + wf, "heedle.run=" + run, "heedle.step=n2"}
	if got := runLabels(t, log); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("agent step: %q, want %q", got, want)
	}

	// A skill step, as treadle runs one (pirn.Step, no node ID of its own).
	env = s.NewTestActivityEnvironment()
	wf, run = execution(t, env)
	podman, log = fakePodman(t, "ok")
	a.Runner = bobbin.Runner{Podman: podman}
	skill := map[string][]byte{"SKILL.md": []byte("---\nname: weather\n---\nSay sunny.")}
	env.RegisterActivityWithOptions(func(ctx context.Context) (string, error) {
		return pirn.Step(ctx, a, pirn.Question{Instruction: "Boston", Skill: skill})
	}, activity.RegisterOptions{Name: "Skill"})
	if _, err := env.ExecuteActivity("Skill"); err != nil {
		t.Fatal(err)
	}
	want = []string{"heedle.kind=skill", "heedle.workflow=" + wf, "heedle.run=" + run}
	if got := runLabels(t, log); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("skill step: %q, want %q", got, want)
	}

	// A step with a local connector: its server and the workspace, both
	// with the run labels the step's context carries.
	t.Setenv("FILES_TOKEN", "files-secret")
	cs := pirn.Connectors{{Name: "files", Image: zeros, Command: []string{"server"}, Secrets: map[string]string{"FAKE_TOKEN": "FILES_TOKEN"}, Writes: []string{"send"}}}
	g, _ := pirn.ParseGrant([]string{"files/search"})
	capi := newAPI(t, turn("end_turn", `{"type":"text","text":"Done."}`))
	podman, log = localPodman(t)
	ca := pirn.Agent{Claude: pirn.Claude{APIKey: "k", BaseURL: capi.srv.URL}, Runner: bobbin.Runner{Podman: podman}, Image: zeros, Connectors: cs}
	ctx := bobbin.WithLabels(context.Background(), bobbin.Labels{Workflow: "loom-x/plan", Run: "r1", Step: "n1"})
	if _, err := ca.Run(ctx, pirn.Question{Instruction: "Find.", Grant: g}); err != nil {
		t.Fatal(err)
	}
	if got, want := runLabels(t, log), [][]string{
		{"heedle.kind=connector", "heedle.workflow=loom-x/plan", "heedle.run=r1", "heedle.step=n1"},
		{"heedle.kind=agent", "heedle.workflow=loom-x/plan", "heedle.run=r1", "heedle.step=n1"},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("connector and workspace: %q, want %q", got, want)
	}

	// A write, run once after the run's steps, in the run's own workflow.
	env = s.NewTestActivityEnvironment()
	wf, run = execution(t, env)
	podman, log = localPodman(t)
	ca.Runner = bobbin.Runner{Podman: podman}
	env.RegisterActivityWithOptions(ca.RunWrite, activity.RegisterOptions{Name: pirn.WriteActivityName})
	if _, err := env.ExecuteActivity(pirn.WriteActivityName, pirn.Write{ID: "w1", Connector: "files", Tool: "send", Input: []byte(`{"query":"x"}`)}); err != nil {
		t.Fatal(err)
	}
	want = []string{"heedle.kind=connector", "heedle.workflow=" + wf, "heedle.run=" + run}
	if got := runLabels(t, log); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("write: %q, want %q", got, want)
	}

	// An act step's browser: reed adds heedle.kind=act to these.
	env = s.NewTestActivityEnvironment()
	wf, run = execution(t, env)
	act, b := newAct(t, newActAPI(t, actTurn("end_turn", `{"type":"text","text":"Nothing to do.\nResult: finished"}`)))
	env.RegisterActivityWithOptions(shuttle.ArrowAsActivity(pirn.ActCommand(act)), activity.RegisterOptions{Name: pirn.ActActivityName})
	if _, err := env.ExecuteActivity(pirn.ActActivityName, plan.Input{NodeID: "n3", Args: []string{"Sign up.", "https://forms.example.org/"}}); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{reed.LabelWorkflow: wf, reed.LabelRun: run, reed.LabelStep: "n3"}; !reflect.DeepEqual(b.got.Labels, want) {
		t.Errorf("act visit labels %v, want %v", b.got.Labels, want)
	}
}

// TestInv_LabelSchemesAgree: bobbin and reed write the same labels: the
// same names, the same values read from Temporal, and the same rule for
// what a value may be (PIRN-501).
func TestInv_LabelSchemesAgree(t *testing.T) {
	if bobbin.LabelKind != reed.LabelKind || bobbin.LabelWorkflow != reed.LabelWorkflow || bobbin.LabelRun != reed.LabelRun || bobbin.LabelStep != reed.LabelStep {
		t.Fatal("bobbin and reed name heedle's labels differently")
	}
	var s testsuite.WorkflowTestSuite
	env := s.NewTestActivityEnvironment()
	env.RegisterActivityWithOptions(func(ctx context.Context, step string) (bool, error) {
		return reflect.DeepEqual(bobbin.StepLabels(ctx, step).Map(), reed.StepLabels(ctx, step)), nil
	}, activity.RegisterOptions{Name: "Agree"})
	for _, step := range []string{"n0", "n-1.a", "--privileged", "a=b", "x y", strings.Repeat("s", 255), strings.Repeat("s", 256), "ünï", "n\n0", "@n", "A:b/c+d"} {
		v, err := env.ExecuteActivity("Agree", step)
		if err != nil {
			t.Fatal(err)
		}
		var same bool
		if v.Get(&same); !same {
			t.Errorf("for step %q, bobbin and reed label differently", step)
		}
	}
}
