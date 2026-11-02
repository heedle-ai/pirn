package pirn

import (
	"context"
	"encoding/json"
	"errors"

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
