package pirn_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestInv_OnlyTheSDKTalksToClaude: pirn's code imports an allowlist. The
// Messages API is reached only through Anthropic's SDK, and pirn opens no
// sockets of its own: no net or net/http, so only the SDK can make a
// request. Only bobbin (Podman) runs an agent's commands. The exceptions
// are registry.go, which alone may run a process and touch files (oras, on
// a temporary copy of a file too big for the record); mcp.go, which alone
// reaches MCP servers, through the official Go SDK (net/http only to give
// that SDK a client that sends a remote server its token); and
// connector.go, which reads tokens from the worker's environment.
func TestInv_OnlyTheSDKTalksToClaude(t *testing.T) {
	allowed := map[string]bool{
		"context": true, "encoding/json": true, "errors": true, "fmt": true,
		"regexp": true, "sort": true, "strconv": true, "strings": true, "time": true,
		"encoding/base64": true, "unicode/utf8": true, "crypto/sha256": true, "encoding/hex": true, "io": true, "bytes": true,
		// Agent steps run their commands only through bobbin's sessions.
		"github.com/heedle-ai/bobbin":            true,
		"github.com/anthropics/anthropic-sdk-go": true, "github.com/anthropics/anthropic-sdk-go/option": true,
		"go.temporal.io/sdk/activity": true, "go.temporal.io/sdk/temporal": true,
		"github.com/heedle-ai/draft/registry": true, "github.com/heedle-ai/reed": true,
		"github.com/heedle-ai/shuttle": true, "github.com/heedle-ai/shuttle/plan": true, "github.com/heedle-ai/weft": true,
	}
	files, _ := filepath.Glob("*.go")
	var bad []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			except := map[string][]string{
				"registry.go":  {"os", "os/exec", "path/filepath"},
				"mcp.go":       {"github.com/modelcontextprotocol/go-sdk/mcp", "net/http", "github.com/anthropics/anthropic-sdk-go/packages/param"},
				"connector.go": {"os", "net/url"},
			}
			if !allowed[p] && !slices.Contains(except[f], p) {
				bad = append(bad, f+": "+p)
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("imports outside the allowlist:\n%s", strings.Join(bad, "\n"))
	}
}

// TestInv_WikiSnippetIsTheExample: every line of the Go snippet on Getting
// Started is a line of ExampleRegister, which the tests compile.
func TestInv_WikiSnippetIsTheExample(t *testing.T) {
	page, err := os.ReadFile("docs/wiki/Getting-Started.md")
	if err != nil {
		t.Fatal(err)
	}
	ex, err := os.ReadFile("example_test.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)```go\n(.*?)```").FindSubmatch(page)
	if m == nil {
		t.Fatal("Getting Started has no Go snippet")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(m[1])), "\n") {
		if !strings.Contains(string(ex), "\t"+line+"\n") {
			t.Errorf("snippet line %q isn't in ExampleRegister", line)
		}
	}
}
