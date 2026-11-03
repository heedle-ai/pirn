package pirn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.temporal.io/sdk/temporal"

	"github.com/heedle-ai/bobbin"
)

// Connector limits, like the agent step's own.
const (
	// MaxToolCalls bounds the connector tool calls one step makes or
	// queues, across all turns.
	MaxToolCalls = 60
	// MaxWrites bounds the writes one step queues.
	MaxWrites = 20
	// MaxWriteInput bounds one queued write's input, as JSON.
	MaxWriteInput = 16 << 10
	// MCPBeta is the Messages API beta that adds remote MCP servers.
	MCPBeta = anthropic.AnthropicBetaMCPClient2025_11_20
	// MaxTools bounds the tools one connector may list, and MaxToolSchema
	// the size of one tool's input schema, as JSON: a server listing more
	// fails the step as setup; a bigger schema leaves that tool out.
	MaxTools      = 100
	MaxToolSchema = 16 << 10
	// MaxServerOutput bounds what pirn reads from one MCP server over a
	// whole step (all its responses), so no server can fill the worker's
	// memory.
	MaxServerOutput = 32 << 20
)

// ToolCall is one connector tool call a step made or queued, as recorded.
type ToolCall struct {
	Connector string `json:"connector"`
	Tool      string `json:"tool"`
	Input     string `json:"input"` // JSON, cut at RecordedOutput
	Output    string `json:"output,omitempty"`
	IsError   bool   `json:"isError,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// Remote is set for a call Anthropic's MCP connector made, from the
	// response; others pirn made, on the worker.
	Remote bool `json:"remote,omitempty"`
	// Write is set for a write: never run in the step, queued as the
	// Write with this ID.
	Write string `json:"write,omitempty"`
}

// Write is a write tool call a step queued: loom runs it once, after the
// run's steps, and only once approved if Approve is set.
type Write struct {
	ID        string          `json:"id"` // w1, w2, … within the step
	Connector string          `json:"connector"`
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input"`
	Approve   bool            `json:"approve,omitempty"`
	// Approved is set by loom, never by a step, when a person approved
	// this write. RunWrite refuses a write whose connector, on the worker
	// running it, says approve, unless it was approved (review 19: the
	// step's output alone decided whether a write needed approval).
	Approved bool `json:"approved,omitempty"`
}

// ListTimeout bounds starting an MCP server, or reaching a remote one,
// and listing its tools (review 19: one that never answered hung the
// step). It's a variable only so tests can shorten it.
var ListTimeout = time.Minute

// QueuedReply is what Claude is told when it calls a write.
const QueuedReply = "Queued as %s: this write runs once, after the run's steps finish%s. Its result isn't available to this step; don't call it again."

// toolbox is the connectors one agent step uses: remote servers and their
// toolsets for the Messages API, and the client tools pirn runs itself
// (a local server's, and every write, which it only queues).
type toolbox struct {
	servers []anthropic.BetaRequestMCPServerURLDefinitionParam
	tools   []anthropic.BetaToolUnionParam
	client  map[string]clientTool // API tool name -> where it runs
	conns   []*mcp.ClientSession
	boxes   []*bobbin.Session
}

type clientTool struct {
	conn    Connector
	tool    string
	session *mcp.ClientSession // nil for a remote connector's write
}

func (t *toolbox) close() {
	for _, c := range t.conns {
		c.Close()
	}
	for _, b := range t.boxes {
		b.Close()
	}
}

// apiName is the Messages API name of a connector's client tool.
func apiName(conn, tool string) string {
	return conn + "__" + strings.ReplaceAll(tool, ".", "_")
}

// openTools starts what grant g needs: for a remote connector, its server
// and an allowlist of exactly the granted tools that aren't writes; for a
// local one, its server in a fresh session, listed over MCP; and for every
// granted write, a client tool that only queues it.
func (a Agent) openTools(ctx context.Context, g Grant) (*toolbox, error) {
	t := &toolbox{client: map[string]clientTool{}}
	ok := false
	defer func() {
		if !ok {
			t.close()
		}
	}()
	for _, name := range g.Connectors() {
		c, _ := a.Connectors.Find(name)
		granted := g[name]
		if c.URL != "" {
			token, err := c.token(ctx, a.Vault)
			if err != nil {
				return nil, err
			}
			srv := anthropic.BetaRequestMCPServerURLDefinitionParam{Name: c.Name, URL: c.URL}
			if token != "" {
				srv.AuthorizationToken = param.NewOpt(token)
			}
			t.servers = append(t.servers, srv)
			set := anthropic.BetaMCPToolsetParam{MCPServerName: c.Name, Configs: map[string]anthropic.BetaMCPToolConfigParam{}}
			if granted.All {
				for _, w := range c.Writes { // writes are never the API's to run
					set.Configs[w] = anthropic.BetaMCPToolConfigParam{Enabled: param.NewOpt(false)}
				}
			} else {
				set.DefaultConfig = anthropic.BetaMCPToolDefaultConfigParam{Enabled: param.NewOpt(false)}
				for n := range granted.Names {
					if !c.IsWrite(n) {
						set.Configs[n] = anthropic.BetaMCPToolConfigParam{Enabled: param.NewOpt(true)}
					}
				}
			}
			t.tools = append(t.tools, anthropic.BetaToolUnionParam{OfMCPToolset: &set})
			var writes []string
			for _, w := range c.Writes {
				if granted.All || granted.Names[w] {
					writes = append(writes, w)
				}
			}
			if len(writes) == 0 {
				continue
			}
			// The write tools' schemas come from the server itself.
			cs, err := connect(ctx, &mcp.StreamableClientTransport{Endpoint: c.URL, HTTPClient: bearer(token, a.MCPTransport), MaxRetries: -1, DisableStandaloneSSE: true})
			if err != nil {
				return nil, fmt.Errorf("connector %q: %w", c.Name, err)
			}
			t.conns = append(t.conns, cs)
			if err := t.add(ctx, c, cs, writes, nil); err != nil {
				return nil, err
			}
			continue
		}
		secrets, err := c.secrets(ctx, a.Vault)
		if err != nil {
			return nil, err
		}
		box, err := a.Runner.Start(ctx, bobbin.Spec{Image: c.Image, Secrets: secrets})
		if err != nil {
			if errors.Is(err, bobbin.ErrInvalid) || errors.Is(err, bobbin.ErrSetup) {
				return nil, fmt.Errorf("%w: connector %q: %v", ErrSetup, c.Name, err)
			}
			return nil, err
		}
		t.boxes = append(t.boxes, box)
		tr, err := serve(ctx, box, c.Command)
		if err != nil {
			return nil, fmt.Errorf("%w: connector %q: its server didn't start: %v", ErrSetup, c.Name, err)
		}
		cs, err := connect(ctx, tr)
		if err != nil {
			return nil, fmt.Errorf("%w: connector %q: its server didn't start: %v", ErrSetup, c.Name, err)
		}
		t.conns = append(t.conns, cs)
		if err := t.add(ctx, c, cs, nil, &granted); err != nil {
			return nil, err
		}
	}
	ok = true
	return t, nil
}

// add offers the server's tools as client tools: those named in only, or
// (with only nil) those granted allows.
func (t *toolbox) add(ctx context.Context, c Connector, cs *mcp.ClientSession, only []string, granted *Tools) error {
	want := map[string]bool{}
	for _, n := range only {
		want[n] = true
	}
	lctx, cancel := context.WithTimeout(ctx, ListTimeout)
	defer cancel()
	listed := 0
	for tool, err := range cs.Tools(lctx, nil) {
		if err != nil {
			return fmt.Errorf("connector %q: listing its tools: %w", c.Name, err)
		}
		if listed++; listed > MaxTools {
			return fmt.Errorf("%w: connector %q lists more than %d tools", ErrSetup, c.Name, MaxTools)
		}
		if only != nil && !want[tool.Name] || granted != nil && !granted.All && !granted.Names[tool.Name] || !toolName.MatchString(tool.Name) {
			continue
		}
		name := apiName(c.Name, tool.Name)
		if _, dup := t.client[name]; dup || len(name) > 64 {
			return fmt.Errorf("%w: connector %q: tool %q can't be offered as %q", ErrSetup, c.Name, tool.Name, name)
		}
		var schema struct {
			Properties any      `json:"properties"`
			Required   []string `json:"required"`
		}
		b, _ := json.Marshal(tool.InputSchema)
		if len(b) > MaxToolSchema {
			continue
		}
		json.Unmarshal(b, &schema)
		desc := clip(tool.Description, 1024)
		if c.IsWrite(tool.Name) {
			desc = clip("A write: it's queued and runs once after this run's steps, not now. "+desc, 1024)
		}
		var session *mcp.ClientSession
		if c.URL == "" || !c.IsWrite(tool.Name) {
			session = cs
		}
		t.client[name] = clientTool{conn: c, tool: tool.Name, session: session}
		t.tools = append(t.tools, anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name: name, Description: param.NewOpt(desc),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: schema.Properties, Required: schema.Required},
		}})
	}
	return nil
}

// connect speaks MCP over a transport, giving the server ListTimeout to
// answer.
func connect(ctx context.Context, tr mcp.Transport) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "pirn", Version: "1"}, nil)
	type result struct {
		cs  *mcp.ClientSession
		err error
	}
	done := make(chan result, 1)
	go func() {
		cs, err := c.Connect(ctx, tr, nil)
		done <- result{cs, err}
	}()
	select {
	case r := <-done:
		return r.cs, r.err
	case <-time.After(ListTimeout):
		go func() { // close it if it ever connects
			if r := <-done; r.cs != nil {
				r.cs.Close()
			}
		}()
		return nil, fmt.Errorf("the server didn't answer within %s", ListTimeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// serve starts a local connector's server in its session and speaks to it
// over standard input and output, reading at most MaxServerOutput from it.
// Closing the transport's connection ends the server.
func serve(ctx context.Context, box *bobbin.Session, argv []string) (mcp.Transport, error) {
	cmd := box.Command(ctx, `exec "$@"`, argv...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go cmd.Wait() // reaps it; the session's end kills it at the latest
	return &mcp.IOTransport{Reader: &bounded{r: out, left: MaxServerOutput}, Writer: in}, nil
}

// bounded reads at most left bytes, then fails.
type bounded struct {
	r    io.ReadCloser
	left int64
}

func (b *bounded) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, fmt.Errorf("the server sent more than %d bytes", MaxServerOutput)
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *bounded) Close() error { return b.r.Close() }

// MCPRoundTripper is an http.RoundTripper, for Agent.MCPTransport.
type MCPRoundTripper = http.RoundTripper

// bearer is an HTTP client that sends token to the server, if any. It
// follows no redirect and sends nothing but https, so the token reaches
// only the connector's own URL (review 19: a redirect carried it to
// another host, over plain http), and it reads at most MaxServerOutput of
// any response.
func bearer(token string, base http.RoundTripper) *http.Client {
	if base == nil {
		base = http.DefaultTransport
	}
	return &http.Client{
		Transport:     bearerTransport{token, base},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("pirn: an MCP server may not redirect") },
	}
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, fmt.Errorf("pirn: an MCP server is reached only over https")
	}
	if b.token != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	res, err := b.base.RoundTrip(r)
	if err == nil {
		res.Body = &bounded{r: res.Body, left: MaxServerOutput}
	}
	return res, err
}

// call runs one client tool call, or queues it if it's a write, records
// it, and returns what Claude sees.
func (a Agent) call(ctx context.Context, t *toolbox, b anthropic.BetaContentBlockUnion, res *AgentResult) (string, bool) {
	ct, ok := t.client[b.Name]
	if !ok {
		return "No such tool.", true
	}
	calls := 0
	for _, c := range res.Tools {
		if !c.Remote {
			calls++
		}
	}
	if calls >= MaxToolCalls {
		return fmt.Sprintf("Not run: this step's limit of %d tool calls is reached. Reply with your summary now.", MaxToolCalls), true
	}
	rec := ToolCall{Connector: ct.conn.Name, Tool: ct.tool, Input: clip(string(b.Input), RecordedOutput), Truncated: len(b.Input) > RecordedOutput}
	if ct.conn.IsWrite(ct.tool) {
		switch {
		case len(res.Writes) >= MaxWrites:
			rec.Output, rec.IsError = fmt.Sprintf("Not queued: this step's limit of %d writes is reached.", MaxWrites), true
		case len(b.Input) > MaxWriteInput || !json.Valid(b.Input):
			rec.Output, rec.IsError = fmt.Sprintf("Not queued: a write's input is JSON of at most %d bytes.", MaxWriteInput), true
		default:
			w := Write{ID: fmt.Sprintf("w%d", len(res.Writes)+1), Connector: ct.conn.Name, Tool: ct.tool, Input: append(json.RawMessage(nil), b.Input...), Approve: ct.conn.Approve}
			// Approved is never a step's to set.
			res.Writes = append(res.Writes, w)
			rec.Write = w.ID
			approval := ""
			if w.Approve {
				approval = ", once a person approves it"
			}
			rec.Output = fmt.Sprintf(QueuedReply, w.ID, approval)
		}
		res.Tools = append(res.Tools, rec)
		a.beat(res)
		return rec.Output, rec.IsError
	}
	cctx, cancel := context.WithTimeout(ctx, a.commandTimeout())
	out, err := ct.session.CallTool(cctx, &mcp.CallToolParams{Name: ct.tool, Arguments: json.RawMessage(b.Input)})
	cancel()
	var text string
	switch {
	case err != nil:
		text, rec.IsError = "The tool call failed: "+err.Error(), true
	default:
		text, rec.IsError = contentText(out.Content), out.IsError
	}
	rec.Output = clip(text, RecordedOutput)
	rec.Truncated = rec.Truncated || len(text) > RecordedOutput
	res.Tools = append(res.Tools, rec)
	a.beat(res)
	if len(text) > ClaudeOutput {
		text = clip(text, ClaudeOutput) + "\n[output cut]"
	}
	return text, rec.IsError
}

// contentText is an MCP result's text; other content is named, not sent.
func contentText(cs []mcp.Content) string {
	var b strings.Builder
	for _, c := range cs {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		} else {
			fmt.Fprintf(&b, "[%T content not shown]", c)
		}
	}
	return b.String()
}

// recordRemote records the calls Anthropic's MCP connector made in one
// response, pairing each result with its call.
func recordRemote(msg *anthropic.BetaMessage, res *AgentResult) {
	at := map[string]int{}
	for _, b := range msg.Content {
		switch b.Type {
		case "mcp_tool_use":
			at[b.ID] = len(res.Tools)
			res.Tools = append(res.Tools, ToolCall{Connector: b.ServerName, Tool: b.Name, Remote: true,
				Input: clip(string(b.Input), RecordedOutput), Truncated: len(b.Input) > RecordedOutput})
		case "mcp_tool_result":
			var r struct {
				ToolUseID string          `json:"tool_use_id"`
				IsError   bool            `json:"is_error"`
				Content   json.RawMessage `json:"content"`
			}
			json.Unmarshal([]byte(b.RawJSON()), &r)
			i, ok := at[r.ToolUseID]
			if !ok {
				continue
			}
			text := mcpResultText(r.Content)
			res.Tools[i].Output, res.Tools[i].IsError = clip(text, RecordedOutput), r.IsError
			res.Tools[i].Truncated = res.Tools[i].Truncated || len(text) > RecordedOutput
		}
	}
}

// mcpResultText is an mcp_tool_result's content: a string, or text blocks.
func mcpResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &blocks)
	var b strings.Builder
	for _, x := range blocks {
		b.WriteString(x.Text)
	}
	return b.String()
}

// WriteActivityName is the activity that runs one queued write: loom runs
// it once per approved write, after a run's steps, never retried.
const WriteActivityName = "PirnWrite"

// WriteResult is what a write's tool returned.
type WriteResult struct {
	Output  string `json:"output"` // cut at RecordedOutput
	IsError bool   `json:"isError,omitempty"`
}

// RunWrite runs one queued write: its connector's tool, with its input,
// once. The connector must still be this worker's and still declare the
// tool a write. Every failure is final ("WriteFailed"): a write is never
// tried twice, so one whose outcome is unknown is reported as failed.
func (a Agent) RunWrite(ctx context.Context, w Write) (WriteResult, error) {
	res, err := a.runWrite(ctx, w)
	if err != nil {
		return WriteResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "WriteFailed", nil)
	}
	return res, nil
}

func (a Agent) runWrite(ctx context.Context, w Write) (WriteResult, error) {
	a, err := a.fresh()
	if err != nil {
		return WriteResult{}, err
	}
	if err := a.Connectors.Check(); err != nil {
		return WriteResult{}, err
	}
	c, ok := a.Connectors.Find(w.Connector)
	if !ok || !c.IsWrite(w.Tool) || !a.Connectors.Grant().Allows(w.Connector, w.Tool) {
		return WriteResult{}, fmt.Errorf("%w: this worker has no write %s/%s", ErrSetup, w.Connector, w.Tool)
	}
	if c.Approve && !w.Approved {
		return WriteResult{}, fmt.Errorf("%w: %s/%s needs a person's approval on this worker, and it wasn't approved", ErrSetup, w.Connector, w.Tool)
	}
	if !json.Valid(w.Input) || len(w.Input) > MaxWriteInput {
		return WriteResult{}, fmt.Errorf("%w: a write's input is JSON of at most %d bytes", ErrInvalid, MaxWriteInput)
	}
	var tr mcp.Transport
	if c.URL != "" {
		token, err := c.token(ctx, a.Vault)
		if err != nil {
			return WriteResult{}, err
		}
		tr = &mcp.StreamableClientTransport{Endpoint: c.URL, HTTPClient: bearer(token, a.MCPTransport), MaxRetries: -1, DisableStandaloneSSE: true}
	} else {
		secrets, err := c.secrets(ctx, a.Vault)
		if err != nil {
			return WriteResult{}, err
		}
		box, err := a.Runner.Start(ctx, bobbin.Spec{Image: c.Image, Secrets: secrets})
		if err != nil {
			return WriteResult{}, err
		}
		defer box.Close()
		if tr, err = serve(ctx, box, c.Command); err != nil {
			return WriteResult{}, err
		}
	}
	cs, err := connect(ctx, tr)
	if err != nil {
		return WriteResult{}, err
	}
	defer cs.Close()
	out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: w.Tool, Arguments: w.Input})
	if err != nil {
		return WriteResult{}, err
	}
	return WriteResult{Output: clip(contentText(out.Content), RecordedOutput), IsError: out.IsError}, nil
}
