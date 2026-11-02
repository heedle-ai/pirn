package pirn_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.temporal.io/sdk/temporal"

	"github.com/heedle-ai/bobbin"
	"github.com/heedle-ai/pirn"
	"github.com/heedle-ai/shuttle/plan"
)

// TestMain lets this test binary stand in for a local MCP server: run with
// PIRN_FAKE_MCP=1, it serves one over standard input and output.
func TestMain(m *testing.M) {
	switch os.Getenv("PIRN_FAKE_MCP") {
	case "1":
		fakeServer().Run(context.Background(), &mcp.StdioTransport{})
		return
	case "many": // more tools than MaxTools
		s := mcp.NewServer(&mcp.Implementation{Name: "many", Version: "1"}, nil)
		for i := 0; i <= pirn.MaxTools; i++ {
			mcp.AddTool(s, &mcp.Tool{Name: fmt.Sprintf("t%d", i)}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
				return nil, nil, nil
			})
		}
		s.Run(context.Background(), &mcp.StdioTransport{})
		return
	case "big": // a tool whose schema is over MaxToolSchema, and a small one
		s := mcp.NewServer(&mcp.Implementation{Name: "big", Version: "1"}, nil)
		props := map[string]any{}
		for i := 0; i < 2000; i++ {
			props[fmt.Sprintf("field_with_a_long_name_%d", i)] = map[string]any{"type": "string"}
		}
		mcp.AddTool(s, &mcp.Tool{Name: "huge", InputSchema: map[string]any{"type": "object", "properties": props}}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
			return nil, nil, nil
		})
		mcp.AddTool(s, &mcp.Tool{Name: "small"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return nil, nil, nil
		})
		s.Run(context.Background(), &mcp.StdioTransport{})
		return
	case "silent": // never answers
		io.Copy(io.Discard, os.Stdin)
		return
	}
	os.Exit(m.Run())
}

// fakeServer has a read (search), a write (send, which appends what it
// sent to $FAKE_SENT_FILE) and a tool no step here is granted (wipe).
// search says which secret it was given.
func fakeServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	type q struct {
		Query string `json:"query"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "search", Description: "Searches."}, func(_ context.Context, _ *mcp.CallToolRequest, in q) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "found " + in.Query + " with " + os.Getenv("FAKE_TOKEN")}}}, nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "send", Description: "Sends an email."}, func(_ context.Context, _ *mcp.CallToolRequest, in q) (*mcp.CallToolResult, any, error) {
		f, err := os.OpenFile(os.Getenv("FAKE_SENT_FILE"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, nil, err
		}
		defer f.Close()
		f.WriteString(in.Query + "\n")
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "sent " + in.Query}}}, nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "wipe", Description: "Deletes everything."}, func(context.Context, *mcp.CallToolRequest, q) (*mcp.CallToolResult, any, error) {
		panic("an ungranted tool ran")
	})
	return s
}

// api is a stand-in Messages API that replies with its turns in order and
// keeps every request's body and beta header.
type api struct {
	srv   *httptest.Server
	mu    sync.Mutex
	reqs  []map[string]any
	betas []string
}

func newAPI(t *testing.T, turns ...string) *api {
	a := &api{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(b, &req)
		a.mu.Lock()
		a.reqs = append(a.reqs, req)
		a.betas = append(a.betas, r.Header.Get("anthropic-beta"))
		i := min(len(a.reqs), len(turns)) - 1
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, turns[i])
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func use(id, name, input string) string {
	return `{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}`
}

// TestInv_ConnectorsAreChecked: a worker's connectors file names each
// connector once, as a remote https:// server or a local pinned image
// with a command, never both; credentials only by environment variable;
// writes among its tools; and refuses unknown fields.
func TestInv_ConnectorsAreChecked(t *testing.T) {
	ok := `{"connectors": [
	  {"name": "linear", "url": "https://mcp.linear.app/mcp", "tokenEnv": "LINEAR_TOKEN", "tools": ["search", "create_issue"], "writes": ["create_issue"], "approve": true},
	  {"name": "files", "image": "` + zeros + `", "command": ["server", "--stdio"], "secrets": {"FAKE_TOKEN": "FILES_TOKEN"}}]}`
	cs, err := pirn.ReadConnectors(strings.NewReader(ok))
	if err != nil || len(cs) != 2 || cs.Grant().String() != "files linear/create_issue linear/search" {
		t.Fatalf("%+v %v", cs, err)
	}
	for _, bad := range []string{
		`{"connectors": [{"name": "x", "url": "http://a.b/mcp"}]}`,
		`{"connectors": [{"name": "x", "url": "https://u:p@a.b/mcp"}]}`,
		`{"connectors": [{"name": "x", "url": "https://a.b", "image": "` + zeros + `", "command": ["s"]}]}`,
		`{"connectors": [{"name": "x"}]}`,
		`{"connectors": [{"name": "x", "image": "alpine:latest", "command": ["s"]}]}`,
		`{"connectors": [{"name": "x", "image": "` + zeros + `"}]}`,
		`{"connectors": [{"name": "x", "url": "https://a.b", "tokenEnv": "not a var"}]}`,
		`{"connectors": [{"name": "x", "url": "https://a.b", "token": "secret-in-the-file"}]}`,
		`{"connectors": [{"name": "x", "url": "https://a.b", "tools": ["a"], "writes": ["b"]}]}`,
		`{"connectors": [{"name": "x", "url": "https://a.b", "approve": true}]}`,
		`{"connectors": [{"name": "x", "url": "https://a.b"}, {"name": "x", "url": "https://c.d"}]}`,
		`{"connectors": [{"name": "X_y", "url": "https://a.b"}]}`,
		`{"connectors": [{"name": "x", "image": "` + zeros + `", "command": ["s"], "secrets": {"A": "$(id)"}}]}`,
	} {
		if _, err := pirn.ReadConnectors(strings.NewReader(bad)); !errors.Is(err, pirn.ErrInvalid) {
			t.Errorf("accepted %s", bad)
		}
	}
}

// TestLaw_StepsGetOnlyWhatTheyAreGranted: a step is offered exactly the
// tools its grant names, narrowed by the worker's: a remote connector's
// granted reads as an allowlist (never a write, never another tool), a
// local connector's granted tools as client tools; a step asking for more
// than the worker grants fails as setup before Claude is asked; and a
// step that asks for nothing is offered no connector at all, with no
// beta header.
func TestLaw_StepsGetOnlyWhatTheyAreGranted(t *testing.T) {
	t.Setenv("LINEAR_TOKEN", "lin-token")
	cs := pirn.Connectors{
		{Name: "linear", URL: "https://mcp.example.com/mcp", TokenEnv: "LINEAR_TOKEN", Tools: []string{"search", "get", "create_issue"}, Writes: []string{"create_issue"}},
		{Name: "github", URL: "https://gh.example.com/mcp"},
	}
	podman, _ := fakePodman(t, "ok")
	run := func(args ...string) (*api, pirn.AgentResult, error) {
		a := newAPI(t, turn("end_turn", `{"type":"text","text":"Done."}`))
		g, err := pirn.ParseGrant(args)
		if err != nil {
			t.Fatal(err)
		}
		res, err := pirn.Agent{Claude: pirn.Claude{APIKey: "k", BaseURL: a.srv.URL}, Runner: bobbin.Runner{Podman: podman}, Image: zeros, Connectors: cs}.
			Run(context.Background(), pirn.Question{Instruction: "Look.", Grant: g})
		return a, res, err
	}
	a, res, err := run("linear/search", "github")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(a.reqs[0])
	var body struct {
		MCPServers []map[string]any `json:"mcp_servers"`
		Tools      []map[string]any `json:"tools"`
	}
	json.Unmarshal(req, &body)
	if a.betas[0] != string(pirn.MCPBeta) || len(body.MCPServers) != 2 || body.MCPServers[1]["name"] != "linear" ||
		body.MCPServers[1]["authorization_token"] != "lin-token" || body.MCPServers[0]["authorization_token"] != nil || res.Grant != "github linear/search" {
		t.Errorf("servers %v, beta %q, grant %q", body.MCPServers, a.betas[0], res.Grant)
	}
	sets := map[string]string{}
	for _, tool := range body.Tools {
		if tool["type"] == "mcp_toolset" {
			b, _ := json.Marshal(tool)
			sets[tool["mcp_server_name"].(string)] = string(b)
		}
	}
	if s := sets["linear"]; !strings.Contains(s, `"default_config":{"enabled":false}`) || !strings.Contains(s, `"search":{"enabled":true}`) ||
		strings.Contains(s, "get") || strings.Contains(s, "create_issue") {
		t.Errorf("linear's toolset: %s", s)
	}
	if s := sets["github"]; strings.Contains(s, "default_config") {
		t.Errorf("github's toolset (all granted, no writes): %s", s)
	}
	for _, more := range [][]string{{"slack"}, {"linear/delete_all"}, {"linear"}} {
		a, _, err := run(more...)
		var setup bool
		if errors.Is(err, pirn.ErrSetup) {
			setup = true
		}
		if !setup || len(a.reqs) != 0 {
			t.Errorf("asking for %v: %v, %d requests", more, err, len(a.reqs))
		}
	}
	a, _, err = run()
	if err != nil || a.betas[0] != "" || a.reqs[0]["mcp_servers"] != nil {
		t.Errorf("no grant: %v, beta %q, servers %v", err, a.betas[0], a.reqs[0]["mcp_servers"])
	}
}

// TestInv_RemoteCallsAreRecorded: every call Anthropic's MCP connector
// made is recorded with its result, as a remote call.
func TestInv_RemoteCallsAreRecorded(t *testing.T) {
	a := newAPI(t, turn("end_turn",
		`{"type":"mcp_tool_use","id":"m1","name":"search","server_name":"github","input":{"q":"bugs"}}`,
		`{"type":"mcp_tool_result","tool_use_id":"m1","is_error":false,"content":[{"type":"text","text":"3 bugs"}]}`,
		`{"type":"text","text":"There are 3 bugs."}`))
	podman, _ := fakePodman(t, "ok")
	res, err := pirn.Agent{Claude: pirn.Claude{APIKey: "k", BaseURL: a.srv.URL}, Runner: bobbin.Runner{Podman: podman}, Image: zeros,
		Connectors: pirn.Connectors{{Name: "github", URL: "https://gh.example.com/mcp"}}}.
		Run(context.Background(), pirn.Question{Instruction: "Count bugs.", Grant: pirn.Grant{"github": {All: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || !res.Tools[0].Remote || res.Tools[0].Connector != "github" || res.Tools[0].Tool != "search" ||
		res.Tools[0].Input != `{"q":"bugs"}` || res.Tools[0].Output != "3 bugs" || res.Text != "There are 3 bugs." {
		t.Errorf("tools %+v, text %q", res.Tools, res.Text)
	}
}

// localPodman is a fake podman whose exec of a connector's server runs
// this test binary as a real MCP server over standard input and output,
// with the secret the session was started with, as podman would put it
// in the container.
func localPodman(t *testing.T) (string, string) { return localPodmanAs(t, "1") }

// localPodmanAs is localPodman with the server in the given mode (see
// TestMain).
func localPodmanAs(t *testing.T, mode string) (string, string) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	self, _ := os.Executable()
	sh := `printf '%s\n' "$*" >> ` + log + `
case "$1" in
run) printf '%s' "$FAKE_TOKEN" > ` + log + `.token; echo id;;
diff) echo '{}';;
exec) case "$*" in
  *'exec "$@"'*) FAKE_TOKEN=$(cat ` + log + `.token) PIRN_FAKE_MCP=` + mode + ` exec "` + self + `";;
  *"find . -type f -print0"*) cat >/dev/null;;
  *) cat >/dev/null;;
  esac;;
esac`
	p := filepath.Join(dir, "podman")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+sh+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p, log
}

// TestInv_LocalConnectorsRunInTheirOwnSession: a local connector's server
// runs in its own bobbin session (never the agent's), with its secrets
// off the command line; its granted tools are offered as client tools and
// run there, recorded; a write is queued, never run, and Claude is told
// so; a tool not granted is neither offered nor run.
func TestInv_LocalConnectorsRunInTheirOwnSession(t *testing.T) {
	t.Setenv("FILES_TOKEN", "files-secret")
	sent := filepath.Join(t.TempDir(), "sent")
	t.Setenv("FAKE_SENT_FILE", sent)
	a := newAPI(t,
		turn("tool_use", use("t1", "files__search", `{"query":"q3"}`), use("t2", "files__send", `{"query":"to boss"}`), use("t3", "files__wipe", `{}`)),
		turn("end_turn", `{"type":"text","text":"Done."}`))
	podman, log := localPodman(t)
	cs := pirn.Connectors{{Name: "files", Image: zeros, Command: []string{"server"}, Secrets: map[string]string{"FAKE_TOKEN": "FILES_TOKEN"},
		Writes: []string{"send"}, Approve: true}}
	g, _ := pirn.ParseGrant([]string{"files/search", "files/send"})
	res, err := pirn.Agent{Claude: pirn.Claude{APIKey: "k", BaseURL: a.srv.URL}, Runner: bobbin.Runner{Podman: podman}, Image: zeros, Connectors: cs}.
		Run(context.Background(), pirn.Question{Instruction: "Find and send.", Grant: g})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range a.reqs[0]["tools"].([]any) {
		if n, ok := tool.(map[string]any)["name"].(string); ok {
			names = append(names, n)
		}
	}
	if strings.Join(names, " ") != "bash files__search files__send" || a.reqs[0]["mcp_servers"] != nil {
		t.Errorf("offered %v, servers %v", names, a.reqs[0]["mcp_servers"])
	}
	if len(res.Tools) != 2 || res.Tools[0].Output != "found q3 with files-secret" || res.Tools[0].Remote ||
		res.Tools[1].Write != "w1" || len(res.Writes) != 1 || res.Writes[0].Tool != "send" || string(res.Writes[0].Input) != `{"query":"to boss"}` || !res.Writes[0].Approve {
		t.Errorf("tools %+v, writes %+v", res.Tools, res.Writes)
	}
	second, _ := json.Marshal(a.reqs[1]["messages"])
	if !strings.Contains(string(second), "Queued as w1") || !strings.Contains(string(second), "No such tool.") {
		t.Errorf("Claude was told: %s", second)
	}
	if b, _ := os.ReadFile(sent); len(b) != 0 {
		t.Errorf("a write ran in the step: %q", b)
	}
	calls, _ := os.ReadFile(log)
	if strings.Count(string(calls), "run --detach --rm") != 2 || strings.Contains(string(calls), "files-secret") || !strings.Contains(string(calls), "--env FAKE_TOKEN") {
		t.Errorf("podman calls:\n%s", calls)
	}
}

// remoteServer serves fakeServer over Streamable HTTP with TLS, and
// records the Authorization headers it got.
func remoteServer(t *testing.T) (*httptest.Server, *[]string) {
	var auth []string
	var mu sync.Mutex
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer() }, nil)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &auth
}

// TestInv_RemoteWritesAreQueued: a remote connector's granted write is
// never the API's to run (it's disabled in the toolset); pirn lists its
// schema from the server with the connector's token and offers it as a
// client tool that only queues it.
func TestInv_RemoteWritesAreQueued(t *testing.T) {
	t.Setenv("MAIL_TOKEN", "mail-token")
	srv, auth := remoteServer(t)
	a := newAPI(t, turn("tool_use", use("t1", "mail__send", `{"query":"hi"}`)), turn("end_turn", `{"type":"text","text":"Queued."}`))
	podman, _ := fakePodman(t, "ok")
	cs := pirn.Connectors{{Name: "mail", URL: srv.URL + "/mcp", TokenEnv: "MAIL_TOKEN", Writes: []string{"send"}}}
	res, err := pirn.Agent{Claude: pirn.Claude{APIKey: "k", BaseURL: a.srv.URL}, Runner: bobbin.Runner{Podman: podman}, Image: zeros,
		Connectors: cs, MCPTransport: srv.Client().Transport}.Run(context.Background(), pirn.Question{Instruction: "Mail.", Grant: pirn.Grant{"mail": {All: true}}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(a.reqs[0]["tools"])
	if !strings.Contains(string(req), `"send":{"enabled":false}`) || !strings.Contains(string(req), `"name":"mail__send"`) || strings.Contains(string(req), "mail__search") {
		t.Errorf("tools: %s", req)
	}
	if len(res.Writes) != 1 || res.Writes[0].Connector != "mail" || res.Writes[0].Approve {
		t.Errorf("writes %+v", res.Writes)
	}
	// Review 19: granted by name, a write is still never the API's.
	b := newAPI(t, turn("end_turn", `{"type":"text","text":"Done."}`))
	g, _ := pirn.ParseGrant([]string{"mail/send", "mail/search"})
	if _, err := (pirn.Agent{Claude: pirn.Claude{APIKey: "k", BaseURL: b.srv.URL}, Runner: bobbin.Runner{Podman: podman}, Image: zeros,
		Connectors: cs, MCPTransport: srv.Client().Transport}).Run(context.Background(), pirn.Question{Instruction: "Mail.", Grant: g}); err != nil {
		t.Fatal(err)
	}
	named, _ := json.Marshal(b.reqs[0]["tools"])
	if !strings.Contains(string(named), `"search":{"enabled":true}`) || strings.Contains(string(named), `"send":{"enabled":true}`) || !strings.Contains(string(named), `"name":"mail__send"`) {
		t.Errorf("tools granted by name: %s", named)
	}
	for _, h := range *auth {
		if h != "Bearer mail-token" {
			t.Errorf("the server got Authorization %q", h)
		}
	}
	if len(*auth) == 0 {
		t.Error("the server was never asked")
	}
}

// TestInv_AgentArgumentsAreGrants: (agent "instruction" "linear/search")
// grants its step that; a malformed grant fails the step as invalid input.
func TestInv_AgentArgumentsAreGrants(t *testing.T) {
	_, err := pirn.AgentCommand(pirn.Agent{})(context.Background(), plan.Input{Args: []string{"Look.", "Not A Grant"}})
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || app.Type() != "InvalidInput" {
		t.Errorf("a bad grant: %v", err)
	}
	if spec := pirn.AgentBuiltinSpec(); !spec.ArgSchema.Variadic {
		t.Error("agent takes no grants")
	}
}

// TestInv_WritesRunOnceAndFailFinally: RunWrite runs a queued write's
// tool once, in a fresh session for a local connector or with the token
// for a remote one, and returns what it said; a write the worker no
// longer declares (or a read, or an unknown connector) is refused, and
// every failure is final ("WriteFailed"), never retried.
func TestInv_WritesRunOnceAndFailFinally(t *testing.T) {
	sent := filepath.Join(t.TempDir(), "sent")
	t.Setenv("FAKE_SENT_FILE", sent)
	t.Setenv("FILES_TOKEN", "files-secret")
	t.Setenv("MAIL_TOKEN", "mail-token")
	podman, _ := localPodman(t)
	srv, auth := remoteServer(t)
	a := pirn.Agent{Runner: bobbin.Runner{Podman: podman}, MCPTransport: srv.Client().Transport, Connectors: pirn.Connectors{
		{Name: "files", Image: zeros, Command: []string{"server"}, Secrets: map[string]string{"FAKE_TOKEN": "FILES_TOKEN"}, Writes: []string{"send"}},
		{Name: "mail", URL: srv.URL + "/mcp", TokenEnv: "MAIL_TOKEN", Writes: []string{"send"}},
	}}
	for _, conn := range []string{"files", "mail"} {
		r, err := a.RunWrite(context.Background(), pirn.Write{ID: "w1", Connector: conn, Tool: "send", Input: []byte(`{"query":"to ` + conn + `"}`)})
		if err != nil || r.Output != "sent to "+conn || r.IsError {
			t.Errorf("%s: %+v %v", conn, r, err)
		}
	}
	if b, _ := os.ReadFile(sent); string(b) != "to files\nto mail\n" {
		t.Errorf("sent %q", b)
	}
	if len(*auth) == 0 || (*auth)[0] != "Bearer mail-token" {
		t.Errorf("auth %q", *auth)
	}
	// Review 19: whether a write needs approval is the worker's to say,
	// not the step's: a connector that says approve runs only an approved
	// write.
	strict := a
	strict.Connectors = append(pirn.Connectors(nil), a.Connectors...)
	strict.Connectors[0].Approve = true
	if _, err := strict.RunWrite(context.Background(), pirn.Write{Connector: "files", Tool: "send", Input: []byte(`{"query":"unapproved"}`)}); err == nil {
		t.Error("a write the worker says needs approval ran unapproved")
	}
	if r, err := strict.RunWrite(context.Background(), pirn.Write{Connector: "files", Tool: "send", Input: []byte(`{"query":"approved"}`), Approved: true}); err != nil || r.Output != "sent approved" {
		t.Errorf("an approved write: %+v %v", r, err)
	}
	for _, bad := range []pirn.Write{
		{Connector: "files", Tool: "search", Input: []byte(`{}`)},
		{Connector: "files", Tool: "wipe", Input: []byte(`{}`)},
		{Connector: "slack", Tool: "send", Input: []byte(`{}`)},
		{Connector: "files", Tool: "send", Input: []byte(`not json`)},
	} {
		_, err := a.RunWrite(context.Background(), bad)
		var app *temporal.ApplicationError
		if !errors.As(err, &app) || app.Type() != "WriteFailed" || !app.NonRetryable() {
			t.Errorf("%s/%s: %v", bad.Connector, bad.Tool, err)
		}
	}
	if b, _ := os.ReadFile(sent); string(b) != "to files\nto mail\napproved\n" {
		t.Errorf("a refused write ran: %q", b)
	}
}

// TestInv_WikiConnectorsFileIsValid: the connectors file on How It Works
// is one ReadConnectors accepts, and its program's grant parses.
func TestInv_WikiConnectorsFileIsValid(t *testing.T) {
	page, err := os.ReadFile("docs/wiki/How-It-Works.md")
	if err != nil {
		t.Fatal(err)
	}
	_, sec, _ := strings.Cut(string(page), "### Connectors")
	_, rest, _ := strings.Cut(sec, "```json\n")
	file, _, _ := strings.Cut(rest, "```")
	cs, err := pirn.ReadConnectors(strings.NewReader(file))
	if err != nil || len(cs) != 2 {
		t.Fatalf("the wiki's connectors file: %v", err)
	}
	if g, err := pirn.ParseGrant([]string{"linear/search_issues", "mail"}); err != nil || cs.Grant().Narrow(g).String() != g.String() || !strings.Contains(sec, `"linear/search_issues" "mail")`) {
		t.Errorf("the wiki's program asks for more than its file grants: %v", err)
	}
}

// TestInv_MisbehavingServersAreBounded: a server that never answers fails
// the step within ListTimeout; one listing more than MaxTools fails it as
// setup; a tool whose schema is over MaxToolSchema is left out. (Review
// 19: a silent server hung the step until its timeout, and a 1 MB schema
// went to Claude.)
func TestInv_MisbehavingServersAreBounded(t *testing.T) {
	old := pirn.ListTimeout
	pirn.ListTimeout = 2 * time.Second
	defer func() { pirn.ListTimeout = old }()
	run := func(mode string) (*api, error) {
		a := newAPI(t, turn("end_turn", `{"type":"text","text":"Done."}`))
		podman, _ := localPodmanAs(t, mode)
		_, err := pirn.Agent{Claude: pirn.Claude{APIKey: "k", BaseURL: a.srv.URL}, Runner: bobbin.Runner{Podman: podman}, Image: zeros,
			Connectors: pirn.Connectors{{Name: "x", Image: zeros, Command: []string{"server"}}}}.
			Run(context.Background(), pirn.Question{Instruction: "Go.", Grant: pirn.Grant{"x": {All: true}}})
		return a, err
	}
	start := time.Now()
	if _, err := run("silent"); err == nil || time.Since(start) > 20*time.Second {
		t.Errorf("a silent server: %v after %s", err, time.Since(start))
	}
	if _, err := run("many"); !errors.Is(err, pirn.ErrSetup) {
		t.Errorf("too many tools: %v", err)
	}
	a, err := run("big")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(a.reqs[0]["tools"])
	if strings.Contains(string(req), "x__huge") || !strings.Contains(string(req), "x__small") || len(req) > pirn.MaxToolSchema {
		t.Errorf("tools sent: %d bytes, huge %v", len(req), strings.Contains(string(req), "x__huge"))
	}
}

// TestInv_TokensStayWithTheirServer: a remote connector's token is sent
// only to its own URL, over https: a redirect elsewhere is refused, never
// followed with the token. (Review 19: a 307 to an http:// host got it.)
func TestInv_TokensStayWithTheirServer(t *testing.T) {
	t.Setenv("MAIL_TOKEN", "mail-token")
	var leaked []string
	var mu sync.Mutex
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Header.Get("Authorization"))
		mu.Unlock()
	}))
	defer other.Close()
	for _, to := range []string{other.URL + "/steal", "https://" + strings.TrimPrefix(other.URL, "http://") + "/steal"} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, to, http.StatusTemporaryRedirect)
		}))
		a := pirn.Agent{MCPTransport: srv.Client().Transport, Connectors: pirn.Connectors{{Name: "mail", URL: srv.URL + "/mcp", TokenEnv: "MAIL_TOKEN", Writes: []string{"send"}}}}
		if _, err := a.RunWrite(context.Background(), pirn.Write{Connector: "mail", Tool: "send", Input: []byte(`{}`)}); err == nil {
			t.Errorf("a redirected write succeeded")
		}
		srv.Close()
	}
	if len(leaked) != 0 {
		t.Errorf("the token went elsewhere: %q", leaked)
	}
}
