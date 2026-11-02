package pirn

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/heedle-ai/draft/registry"
	"github.com/heedle-ai/reed"
	"github.com/heedle-ai/shuttle"
	"github.com/heedle-ai/shuttle/plan"
	"github.com/heedle-ai/weft"
)

const (
	// CommandName is the draft command: (ask "instruction").
	CommandName = "ask"
	// ActivityName is its Temporal activity.
	ActivityName = "Ask"
)

// Command is the ask step: Args[0] is the instruction, and every upstream
// output is a source. Its output is the Answer as JSON.
func Command(a Asker) weft.Arrow[plan.Input, string] {
	return func(ctx context.Context, in plan.Input) (string, error) {
		if len(in.Args) != 1 {
			return "", temporal.NewNonRetryableApplicationError("ask needs exactly one instruction", "InvalidInput", nil)
		}
		q := Question{Instruction: in.Args[0], Sources: sources(in.Upstream, pageSource)}
		ans, err := a.Ask(ctx, q)
		var wait *RetryAfter
		switch {
		case ctx.Err() != nil && err != nil:
			return "", err // cancelled: say so, never a setup error
		case errors.Is(err, ErrInvalid):
			return "", temporal.NewNonRetryableApplicationError(err.Error(), "InvalidInput", nil)
		case errors.Is(err, ErrSetup):
			return "", temporal.NewNonRetryableApplicationError(err.Error(), "AskSetup", nil)
		case errors.As(err, &wait):
			// Retryable, after the delay the API asked for.
			return "", temporal.NewApplicationErrorWithOptions(err.Error(), "RateLimited", temporal.ApplicationErrorOptions{NextRetryDelay: wait.Delay})
		case err != nil:
			return "", err // retryable: rate limits, overload, network
		}
		b, err := json.Marshal(ans)
		return string(b), err
	}
}

func pageSource(out string) (Source, bool) {
	p, ok := reed.ParsePage(out)
	if !ok {
		return Source{}, false
	}
	return Source{URL: p.URL, Title: p.Title, Text: p.Text}, true
}

// ParseAnswer reads an ask output back. ok is false when out isn't one:
// format 1, naming the model that wrote it.
func ParseAnswer(out string) (a Answer, ok bool) {
	if json.Unmarshal([]byte(out), &a) != nil || a.Pirn != 1 || a.Model == "" {
		return Answer{}, false
	}
	return a, true
}

// BuiltinSpec is the draft builtin: exactly one string, the instruction.
func BuiltinSpec() registry.BuiltinSpec {
	return registry.BuiltinSpec{
		Name:      CommandName,
		AgentID:   ActivityName,
		ArgSchema: registry.ArgSchema{Params: []registry.ParamSpec{{Name: "instruction", Type: registry.StringT}}},
		Backends:  []registry.Backend{registry.BackendTemporal},
	}
}

// Activities is the part of a Temporal worker Register needs.
type Activities interface {
	RegisterActivityWithOptions(a any, options activity.RegisterOptions)
}

// Register adds ask to a draft registry and its activity to a worker.
func Register(reg *registry.Registry, w Activities, a Asker) error {
	if err := reg.Register(BuiltinSpec()); err != nil {
		return err
	}
	w.RegisterActivityWithOptions(shuttle.ArrowAsActivity(Command(a)), activity.RegisterOptions{Name: ActivityName})
	return nil
}

const (
	// AgentCommandName is the draft command: (agent "instruction").
	AgentCommandName = "agent"
	// AgentActivityName is its Temporal activity.
	AgentActivityName = "Agent"
)

// AgentOptions are how agent steps run: long enough for an agent to work,
// with a heartbeat after every command, so a hung step is retried before
// its timeout. They're written into each agent node of a plan.
var AgentOptions = plan.Options{Timeout: 20 * time.Minute, Heartbeat: 5 * time.Minute}

// AgentCommand is the agent step: Args[0] is the instruction, any more
// arguments are the connectors and tools it may use ("linear",
// "linear/search_issues": see ParseGrant), and every upstream output is an
// input file. Its output is the AgentResult as JSON. After every command
// it heartbeats its progress to Temporal.
func AgentCommand(a Agent) weft.Arrow[plan.Input, string] {
	return func(ctx context.Context, in plan.Input) (string, error) {
		a := a // each step gets its own copy: steps run concurrently
		if len(in.Args) < 1 {
			return "", temporal.NewNonRetryableApplicationError("agent needs an instruction", "InvalidInput", nil)
		}
		g, err := ParseGrant(in.Args[1:])
		if err != nil {
			return "", temporal.NewNonRetryableApplicationError(err.Error(), "InvalidInput", nil)
		}
		return Step(ctx, a, Question{Instruction: in.Args[0], Sources: sources(in.Upstream, pageSource), Grant: g})
	}
}

// Sources are a step's upstream outputs as an agent's inputs, in node
// order: a browse step's page as its URL, title and text.
func Sources(upstream map[string]string) []Source { return sources(upstream, pageSource) }

// Step runs one agent step on a, heartbeating to Temporal after every
// command when ctx is an activity's, and returns the AgentResult as JSON.
// Errors are classified for Temporal as AgentCommand's are. Other agent
// steps, such as a skill's, run through it too.
func Step(ctx context.Context, a Agent, q Question) (string, error) {
	if activity.IsActivity(ctx) {
		a.OnProgress = func(r AgentResult) { activity.RecordHeartbeat(ctx, len(r.Commands)) }
	}
	res, err := a.Run(ctx, q)
	var wait *RetryAfter
	switch {
	case ctx.Err() != nil && err != nil:
		return "", err
	case errors.Is(err, ErrInvalid):
		return "", temporal.NewNonRetryableApplicationError(err.Error(), "InvalidInput", nil)
	case errors.Is(err, ErrSetup):
		return "", temporal.NewNonRetryableApplicationError(err.Error(), "AgentSetup", nil)
	case errors.As(err, &wait):
		return "", temporal.NewApplicationErrorWithOptions(err.Error(), "RateLimited", temporal.ApplicationErrorOptions{NextRetryDelay: wait.Delay})
	case err != nil:
		return "", err // retryable, in a fresh session
	}
	b, err := json.Marshal(res)
	return string(b), err
}

// AgentBuiltinSpec is the draft builtin: the instruction, then any
// connectors and tools the step may use, with AgentOptions.
func AgentBuiltinSpec() registry.BuiltinSpec {
	return registry.BuiltinSpec{
		Name:      AgentCommandName,
		AgentID:   AgentActivityName,
		ArgSchema: registry.ArgSchema{Params: []registry.ParamSpec{{Name: "instruction", Type: registry.StringT}}, Variadic: true, VariadicType: registry.StringT},
		Backends:  []registry.Backend{registry.BackendTemporal},
		Options:   AgentOptions,
	}
}

// RegisterAgent adds agent to a draft registry, and its activity and the
// activity that runs its queued writes (WriteActivityName, for loom) to a
// worker. The write activity is no draft command: no program can call it.
func RegisterAgent(reg *registry.Registry, w Activities, a Agent) error {
	if err := reg.Register(AgentBuiltinSpec()); err != nil {
		return err
	}
	w.RegisterActivityWithOptions(shuttle.ArrowAsActivity(AgentCommand(a)), activity.RegisterOptions{Name: AgentActivityName})
	w.RegisterActivityWithOptions(a.RunWrite, activity.RegisterOptions{Name: WriteActivityName})
	return nil
}

// ParseAgent reads an agent output back. ok is false when out isn't one.
func ParseAgent(out string) (r AgentResult, ok bool) {
	if json.Unmarshal([]byte(out), &r) != nil || r.Agent != 1 || r.Pirn != 1 || r.Model == "" {
		return AgentResult{}, false
	}
	return r, true
}
