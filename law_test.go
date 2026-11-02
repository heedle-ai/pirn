package pirn_test

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"github.com/heedle-ai/pirn"
	"github.com/heedle-ai/shuttle/plan"
	"github.com/heedle-ai/weft"
)

// echoAsker is a deterministic stand-in for Claude: its answer is the
// prompt it was sent, so equal answers mean equal prompts.
type echoAsker struct{}

func (echoAsker) Ask(_ context.Context, q pirn.Question) (pirn.Answer, error) {
	p, err := pirn.Prompt(q)
	return pirn.Answer{Pirn: 1, Model: "echo", Text: p}, err
}

func randomInput(r *rand.Rand) plan.Input {
	in := plan.Input{NodeID: "n9", Args: []string{fmt.Sprintf("instruction %d", r.Intn(1000))}, Upstream: map[string]string{}}
	for i := 0; i < r.Intn(6); i++ {
		in.Upstream[fmt.Sprintf("n%d", r.Intn(12))] = fmt.Sprintf("output %d", r.Intn(1000))
	}
	if r.Intn(5) == 0 {
		in.Args = nil // an invalid ask: the laws must hold for failures too
	}
	return in
}

func run(a weft.Arrow[plan.Input, string], in plan.Input) (string, string) {
	out, err := a(context.Background(), in)
	if err != nil {
		return "", err.Error()
	}
	return out, ""
}

// TestLaw_AskIsAWeftArrow: ask is a weft arrow like every other step, so
// weft's identity laws hold for it, on answers and on failures:
// Compose(Id, ask) ≡ ask ≡ Compose(ask, Id). (Claude's answers aren't
// deterministic; the laws are about composition, and hold up to the answer
// that is recorded. A deterministic stand-in makes them checkable.)
func TestLaw_AskIsAWeftArrow(t *testing.T) {
	ask := pirn.Command(echoAsker{})
	left := weft.Compose(weft.Id[plan.Input](), ask)
	right := weft.Compose(ask, weft.Id[string]())
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 200; i++ {
		in := randomInput(r)
		o, e := run(ask, in)
		lo, le := run(left, in)
		ro, re := run(right, in)
		if o != lo || e != le || o != ro || e != re {
			t.Fatalf("input %+v: ask = (%q, %q), Id;ask = (%q, %q), ask;Id = (%q, %q)", in, o, e, lo, le, ro, re)
		}
	}
}

// TestLaw_PromptIsAFunctionOfInputs: the question ask sends depends only on
// the instruction and the upstream outputs, never on the order Go happens
// to visit them in, so the same step with the same inputs always asks the
// same thing.
func TestLaw_PromptIsAFunctionOfInputs(t *testing.T) {
	ask := pirn.Command(echoAsker{})
	r := rand.New(rand.NewSource(11))
	for i := 0; i < 200; i++ {
		in := randomInput(r)
		in.Args = []string{"x"}
		want, _ := run(ask, in)
		for j := 0; j < 10; j++ {
			// Rebuild the map in a different insertion order.
			keys := reflect.ValueOf(in.Upstream).MapKeys()
			r.Shuffle(len(keys), func(a, b int) { keys[a], keys[b] = keys[b], keys[a] })
			again := plan.Input{NodeID: in.NodeID, Args: in.Args, Upstream: map[string]string{}}
			for _, k := range keys {
				again.Upstream[k.String()] = in.Upstream[k.String()]
			}
			if got, _ := run(ask, again); got != want {
				t.Fatalf("same inputs, different questions:\n%s\n---\n%s", want, got)
			}
		}
	}
}
