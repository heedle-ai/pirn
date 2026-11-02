package pirn

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/heedle-ai/bobbin"
)

// A Connector is an MCP server a worker offers agent steps: remote (URL,
// reached through Anthropic's MCP connector) or local (Image and Command,
// run in a bobbin session with no network and spoken to over standard
// input and output). Its credentials are named, never written: TokenEnv
// and Secrets name variables of the worker's environment, read when a
// step starts, so no plan, record or log holds them.
type Connector struct {
	Name string `json:"name"`
	// Remote: an https:// MCP server, and the worker's variable holding
	// its OAuth token, if it needs one.
	URL      string `json:"url,omitempty"`
	TokenEnv string `json:"tokenEnv,omitempty"`
	// Local: a content-pinned image, the command that starts the server
	// in it, and its secrets (container variable -> worker variable).
	Image   string            `json:"image,omitempty"`
	Command []string          `json:"command,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
	// Tools, if set, are the only tools steps may use (the worker's own
	// grant); empty means all of them.
	Tools []string `json:"tools,omitempty"`
	// Writes are the tools that change something outside the run, such
	// as sending an email. A step never runs one: it queues it, and loom
	// runs it once after the run's steps, so a retry never repeats it.
	Writes []string `json:"writes,omitempty"`
	// Approve makes this connector's writes wait for a person's approval.
	Approve bool `json:"approve,omitempty"`
}

// Connectors are a worker's connectors.
type Connectors []Connector

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ReadConnectors reads a worker's connectors file:
// {"connectors": [{"name": "linear", "url": "https://…", …}, …]}, refusing
// unknown fields and anything Check refuses.
func ReadConnectors(r io.Reader) (Connectors, error) {
	var f struct {
		Connectors Connectors `json:"connectors"`
	}
	dec := json.NewDecoder(io.LimitReader(r, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: connectors file: %v", ErrInvalid, err)
	}
	return f.Connectors, f.Connectors.Check()
}

// Check reports whether cs can be offered: unique names steps can grant;
// each remote (an https:// URL without credentials in it) or local (a
// content-pinned image and a command), never both; credentials named
// as environment variables; tools and writes named as MCP tools, and
// every write among the tools when tools are listed.
func (cs Connectors) Check() error {
	seen := map[string]bool{}
	for _, c := range cs {
		bad := func(format string, a ...any) error {
			return fmt.Errorf("%w: connector %q: "+format, append([]any{ErrInvalid, c.Name}, a...)...)
		}
		if !connectorName.MatchString(c.Name) {
			return bad("a name is lower-case letters, digits and hyphens, at most 24")
		}
		if seen[c.Name] {
			return bad("named twice")
		}
		seen[c.Name] = true
		switch {
		case c.URL != "" && c.Image != "":
			return bad("is remote (url) or local (image), not both")
		case c.URL != "":
			u, err := url.Parse(c.URL)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				return bad("url must be https://, without credentials")
			}
			if c.TokenEnv != "" && !envName.MatchString(c.TokenEnv) {
				return bad("tokenEnv must name an environment variable")
			}
			if len(c.Command) > 0 || len(c.Secrets) > 0 {
				return bad("command and secrets are for local connectors")
			}
		case c.Image != "":
			if err := (bobbin.Spec{Image: c.Image}).Validate(); err != nil {
				return bad("%v", err)
			}
			if len(c.Command) == 0 {
				return bad("a local connector needs the command that starts its server")
			}
			for _, a := range c.Command {
				if a == "" || strings.ContainsRune(a, 0) {
					return bad("command arguments can't be empty or hold NUL")
				}
			}
			for k, v := range c.Secrets {
				if !envName.MatchString(k) || !envName.MatchString(v) {
					return bad("secrets map container variables to the worker's variables, by name")
				}
			}
			if c.TokenEnv != "" {
				return bad("tokenEnv is for remote connectors; give a local one secrets")
			}
		default:
			return bad("needs a url (remote) or an image (local)")
		}
		tools := map[string]bool{}
		for _, t := range c.Tools {
			if !toolName.MatchString(t) {
				return bad("tool %q", t)
			}
			tools[t] = true
		}
		for _, w := range c.Writes {
			if !toolName.MatchString(w) {
				return bad("write %q", w)
			}
			if len(c.Tools) > 0 && !tools[w] {
				return bad("write %q isn't one of its tools", w)
			}
		}
		if c.Approve && len(c.Writes) == 0 {
			return bad("approve is for connectors with writes")
		}
	}
	return nil
}

// Grant is the most cs let any step use: each connector with its listed
// tools, or all of them.
func (cs Connectors) Grant() Grant {
	g := Grant{}
	for _, c := range cs {
		if len(c.Tools) == 0 {
			g[c.Name] = Tools{All: true}
			continue
		}
		names := map[string]bool{}
		for _, t := range c.Tools {
			names[t] = true
		}
		g[c.Name] = Tools{Names: names}
	}
	return g
}

// Find is the connector named name.
func (cs Connectors) Find(name string) (Connector, bool) {
	for _, c := range cs {
		if c.Name == name {
			return c, true
		}
	}
	return Connector{}, false
}

// IsWrite reports whether tool is one of c's writes.
func (c Connector) IsWrite(tool string) bool {
	for _, w := range c.Writes {
		if w == tool {
			return true
		}
	}
	return false
}

// token is a remote connector's token, from the worker's environment.
func (c Connector) token() (string, error) {
	if c.TokenEnv == "" {
		return "", nil
	}
	v := os.Getenv(c.TokenEnv)
	if v == "" {
		return "", fmt.Errorf("%w: connector %q needs %s set where the worker runs", ErrSetup, c.Name, c.TokenEnv)
	}
	return v, nil
}

// secrets are a local connector's secrets, from the worker's environment.
func (c Connector) secrets() (map[string]string, error) {
	out := map[string]string{}
	for k, from := range c.Secrets {
		v := os.Getenv(from)
		if v == "" {
			return nil, fmt.Errorf("%w: connector %q needs %s set where the worker runs", ErrSetup, c.Name, from)
		}
		out[k] = v
	}
	return out, nil
}

// grantFor is the grant a step gets: what it asks for, which the worker
// must cover. Asking for more is a setup error, never silently less.
func (cs Connectors) grantFor(asked Grant) (Grant, error) {
	got := cs.Grant().Narrow(asked)
	if got.String() != asked.String() {
		return nil, fmt.Errorf("%w: the step asks for %q, but this worker grants only %q of it (its connectors: %q)", ErrSetup, asked, got, cs.Grant())
	}
	return got, nil
}
