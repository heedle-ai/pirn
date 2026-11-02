package pirn_test

import (
	"os"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/heedle-ai/draft"
	"github.com/heedle-ai/pirn"
)

// ExampleRegister is the snippet on the wiki's Getting Started page. It is
// compiled, not run: it needs a Temporal server.
func ExampleRegister() {
	var c client.Client // from client.Dial
	reg := draft.DefaultRegistry()
	w := worker.New(c, "loom", worker.Options{})
	err := pirn.Register(reg, w, pirn.Claude{APIKey: os.Getenv("ANTHROPIC_API_KEY")})
	_ = err
}
