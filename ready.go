package pirn

import (
	"context"
	"sort"
)

// A Need is one connector a step asks for, and whether this worker can
// give it now. Problem is "" when it can; else why not:
//
//   - "absent": the worker has no connector of that name;
//   - "tools": it has one, but it doesn't allow every tool asked for;
//   - "key": a key the connector needs (Key) isn't set;
//   - "unreadable": a key couldn't be read now (Detail says why), such as a
//     keychain that is locked or waiting for you to allow access.
//
// Ready decides it as a step that starts now would: the same grant
// (grantFor) and the same places for keys (secretFor), so a proposal is
// never called ready when its run would fail for want of a connector.
type Need struct {
	Connector string   `json:"connector"`
	Tools     []string `json:"tools,omitempty"` // the tools asked for, if not all
	Problem   string   `json:"problem,omitempty"`
	Key       string   `json:"key,omitempty"`
	Detail    string   `json:"detail,omitempty"`
}

// Ready is, for each connector asked names, whether cs (with keys from the
// environment, then v) can give it to a step starting now, sorted by name.
func (cs Connectors) Ready(ctx context.Context, asked Grant, v Vault) []Need {
	have := cs.Grant()
	out := []Need{}
	for _, name := range asked.Connectors() {
		n := Need{Connector: name}
		t := asked[name]
		if !t.All {
			for tool := range t.Names {
				n.Tools = append(n.Tools, tool)
			}
			sort.Strings(n.Tools)
		}
		c, ok := cs.Find(name)
		switch {
		case !ok:
			n.Problem = "absent"
		case have.Narrow(Grant{name: t}).String() != (Grant{name: t}).String():
			n.Problem = "tools"
		default:
			for _, k := range c.keyNames() {
				if p, d := KeyReady(ctx, v, k); p != "" {
					n.Problem, n.Key, n.Detail = p, k, d
					break
				}
			}
		}
		out = append(out, n)
	}
	return out
}

// keyNames are the keys c needs at a step's start, sorted.
func (c Connector) keyNames() []string {
	var out []string
	if c.TokenEnv != "" {
		out = append(out, c.TokenEnv)
	}
	for _, from := range c.Secrets {
		out = append(out, from)
	}
	sort.Strings(out)
	return out
}

// KeyReady reports whether the key name can be read now, as a step would
// read it (the environment, then v): "" if so, "key" if it isn't set, or
// "unreadable" with why it couldn't be read.
func KeyReady(ctx context.Context, v Vault, name string) (problem, detail string) {
	if getenv(name) != "" {
		return "", ""
	}
	if v == nil {
		return "key", ""
	}
	if _, ok, err := v.Lookup(ctx, name); err != nil {
		return "unreadable", err.Error()
	} else if !ok {
		return "key", ""
	}
	return "", ""
}
