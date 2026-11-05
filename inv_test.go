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
// that SDK a client that sends a remote server its token); act.go, which
// starts an act step's Playwright MCP in reed's container (os/exec) and
// speaks MCP to it with the same SDK (net/url reads its blocked requests;
// roadmap 9.5); connector.go,
// which reads tokens from the worker's environment; and vault.go, which
// alone runs the keychain's own command (security or secret-tool) and
// stty, to type a secret unseen.
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
				"vault.go":     {"os", "os/exec", "runtime", "sync"},
				"act.go":       {"github.com/modelcontextprotocol/go-sdk/mcp", "net/url", "os/exec", "crypto/rand", "sync"}, // crypto/rand and sync: a pending send's ID and the paused budget (roadmap 9.7)
				"heartbeat.go": {"sync"},
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

// TestInv_CIChecksCanFail: every workflow runs steps with bash -eo
// pipefail (shell: bash), so a failing command in a pipe fails its step,
// and no command is negated ("! grep ..."), which errexit ignores.
func TestInv_CIChecksCanFail(t *testing.T) {
	files, _ := filepath.Glob(".github/workflows/*.yml")
	negated := regexp.MustCompile(`(^|&&|\|\||;|\bthen|\bdo)\s*!\s*[A-Za-z]`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "\ndefaults:\n  run:\n") || !strings.Contains(string(b), "\n    shell: bash\n") {
			t.Errorf("%s: no defaults run shell: bash, so pipes hide failures", f)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if l := strings.TrimSpace(line); !strings.HasPrefix(l, "#") && negated.MatchString(l) {
				t.Errorf("%s:%d: a negated command never fails a step: %s", f, i+1, l)
			}
		}
	}
}

// TestInv_AgentImageIsTestedBeforeItIsPublished: the agent image is built
// on both Linux architectures, each passes the agent container tests
// (ci:image-tested) before it is saved, and only a job that needs both
// builds may push, by digest with a manifest list. (roadmap 9.1)
func TestInv_AgentImageIsTestedBeforeItIsPublished(t *testing.T) {
	b, err := os.ReadFile(".github/workflows/image.yml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"runner: ubuntu-24.04 ", "runner: ubuntu-24.04-arm ", "\n  publish:\n    needs: build\n", "podman manifest push --all --digestfile", "-f images/agent/Containerfile"} {
		if !strings.Contains(s, want) {
			t.Errorf("image.yml lacks %q", want)
		}
	}
	publish := strings.Index(s, "\n  publish:\n")
	order := []string{"- name: build\n", "- name: image-tested\n", "- name: save\n", "actions/upload-artifact"}
	last := -1
	for _, step := range order {
		i := strings.Index(s, step)
		if i < 0 || i < last || (publish >= 0 && i > publish) {
			t.Errorf("image.yml: the build job must run %q in this order, before publish: %v", step, order)
		}
		last = i
	}
	for _, w := range []string{"packages: write", "podman login", "manifest push"} {
		if i := strings.Index(s, w); i < 0 || publish < 0 || i < publish {
			t.Errorf("image.yml: %q may appear only in the publish job", w)
		}
	}
	if strings.Contains(s, "podman push") {
		t.Error("image.yml: images are pushed only with their manifest list (podman manifest push --all)")
	}
	// Review 36: nothing may let a step or job be skipped or its failure
	// ignored, which these string checks alone would not notice.
	for _, bad := range []string{"continue-on-error", "|| true", "|| :", "if: ", "${{ always", "${{ failure", "if-no-files-found: ignore", "if-no-files-found: warn"} {
		if strings.Contains(s, bad) {
			t.Errorf("image.yml uses %q, which could publish an untested image or hide a failure", bad)
		}
	}
	if !strings.Contains(s, "fail-fast: true") || !strings.Contains(s, "if-no-files-found: error") {
		t.Error("image.yml: both architectures must build, and both archives must exist, or nothing is published")
	}
	// Review 36, round 2: the image the tests run against must be the one
	// built, and the one saved and published; the tests must require Podman,
	// fail on a skip, and each expected pass must be checked.
	build := s[:max(publish, 0)]
	for _, bad := range []string{"||", "; true", "--from"} {
		if strings.Contains(build, bad) {
			t.Errorf("image.yml's build job uses %q", bad)
		}
	}
	want := map[string][]string{
		"build": {"id=$(podman build --quiet ", `echo "PIRN_IMAGE=$id" >> "$GITHUB_ENV"` + "\n"},
		"image-tested": {
			`PIRN_REQUIRE_PODMAN: "1"` + "\n",
			"go test -count=1 -v -run '^(TestContainer_AgentInARealSession|TestContainer_AgentKeepsAwkwardFiles|TestContainer_AgentFollowsASkill)$' . | tee image-tested.log\n",
			"if grep -q -- '--- SKIP' image-tested.log; then echo 'a test skipped'; exit 1; fi\n",
			"grep -q -- '--- PASS: TestContainer_AgentInARealSession' image-tested.log\n",
			"grep -q -- '--- PASS: TestContainer_AgentKeepsAwkwardFiles' image-tested.log\n",
			"grep -q -- '--- PASS: TestContainer_AgentFollowsASkill' image-tested.log\n",
		},
		"save":   {`"$PIRN_IMAGE"` + "\n"},
		"podman": {"--add-subuids 100000-165535 --add-subgids 100000-165535"},
	}
	for name, lines := range want {
		step := agentImageStep(build, name)
		for _, l := range lines {
			if !strings.Contains(step, l) {
				t.Errorf("image.yml's %s step lacks %q", name, strings.TrimSpace(l))
			}
		}
	}
	if regexp.MustCompile(`(?:docker\.io/|debian:)`).MatchString(s) {
		t.Error("image.yml names a base image; the agent image's base is its Containerfile's, pinned by digest")
	}
}

// TestInv_AgentImageIsPinnedAndUnprivileged: the agent image's base is
// pinned by digest, it runs as an unprivileged user that owns /work, and
// every package it installs is listed, with why, on Getting Started.
// (roadmap 9.1)
func TestInv_AgentImageIsPinnedAndUnprivileged(t *testing.T) {
	b, err := os.ReadFile("images/agent/Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	cf := string(b)
	if !regexp.MustCompile(`(?m)^FROM docker\.io/library/debian@sha256:[0-9a-f]{64}$`).MatchString(cf) || strings.Count(cf, "\nFROM ") != 1 {
		t.Error("the Containerfile must have one FROM, a Debian image pinned by digest")
	}
	if strings.Contains(cf, ":latest") {
		t.Error("the Containerfile uses :latest")
	}
	for _, want := range []string{"useradd --uid 1000 ", "chown agent:agent /work", "\nUSER agent\n", "\nWORKDIR /work\n"} {
		if !strings.Contains(cf, want) {
			t.Errorf("the Containerfile lacks %q", want)
		}
	}
	if i, j := strings.Index(cf, "\nUSER agent\n"), strings.LastIndex(cf, "\nRUN "); i < j {
		t.Error("the Containerfile runs something as root after USER agent")
	}
	m := regexp.MustCompile(`--no-install-recommends ([a-z0-9. +-]+?) \\`).FindStringSubmatch(cf)
	if m == nil {
		t.Fatal("no apt-get install --no-install-recommends line")
	}
	gs, _ := os.ReadFile("docs/wiki/Getting-Started.md")
	for _, p := range strings.Fields(m[1]) {
		if !strings.Contains(string(gs), "| `"+p+"` |") {
			t.Errorf("package %s isn't listed, with why, on Getting Started", p)
		}
	}
}

// agentImageStep is the text of the step called name in a workflow's job, up to
// the next step.
func agentImageStep(job, name string) string {
	i := strings.Index(job, "- name: "+name+"\n")
	if i < 0 {
		return ""
	}
	rest := job[i+len("- name: "+name+"\n"):]
	if j := strings.Index(rest, "\n      - "); j >= 0 {
		rest = rest[:j+1]
	}
	return rest
}
