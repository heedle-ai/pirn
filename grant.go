package pirn

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// A Grant is what a step may use: connectors by name, each with all its
// tools or only some. Grants only ever narrow: a step gets the
// intersection of every grant above it (the worker's, then the step's
// own), never more than any of them (Narrow). A grant is a list of
// caveats in that sense, each one removing, so it maps onto macaroons
// when they are needed.
type Grant map[string]Tools

// Tools is the tools of one connector a grant allows: every tool (All),
// or only those named.
type Tools struct {
	All   bool
	Names map[string]bool
}

// connectorName and toolName are the names grants use. A connector's
// name, joined to a tool's by "__", must be a Messages API tool name
// (at most 64 characters of letters, digits, "_" and "-"); toolName
// allows "." too, as MCP servers name tools that way.
var (
	connectorName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,22}[a-z0-9])?$`)
	toolName      = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// ParseGrant reads a step's grant from its arguments: "linear" grants
// every tool of the connector linear, "linear/search_issues" one of them.
// Naming a connector twice adds tools; naming it whole grants all.
func ParseGrant(args []string) (Grant, error) {
	g := Grant{}
	for _, a := range args {
		conn, tool, hasTool := strings.Cut(a, "/")
		if !connectorName.MatchString(conn) || hasTool && !toolName.MatchString(tool) {
			return nil, fmt.Errorf("%w: %q isn't a connector or connector/tool", ErrInvalid, a)
		}
		t := g[conn]
		if !hasTool {
			t = Tools{All: true}
		} else if !t.All {
			if t.Names == nil {
				t.Names = map[string]bool{}
			}
			t.Names[tool] = true
		}
		g[conn] = t
	}
	return g, nil
}

// Allows reports whether g lets a step use connector's tool.
func (g Grant) Allows(connector, tool string) bool {
	t, ok := g[connector]
	return ok && (t.All || t.Names[tool])
}

// Narrow is what both g and h allow: a connector both name, with the
// tools both allow. It is commutative, associative and idempotent, and
// allows a tool exactly when both do.
func (g Grant) Narrow(h Grant) Grant {
	out := Grant{}
	for conn, a := range g {
		b, ok := h[conn]
		if !ok {
			continue
		}
		switch {
		case a.All && b.All:
			out[conn] = Tools{All: true}
		case a.All:
			out[conn] = b.clone()
		case b.All:
			out[conn] = a.clone()
		default:
			names := map[string]bool{}
			for n := range a.Names {
				if b.Names[n] {
					names[n] = true
				}
			}
			if len(names) > 0 {
				out[conn] = Tools{Names: names}
			}
		}
	}
	return out
}

func (t Tools) clone() Tools {
	if t.All {
		return Tools{All: true}
	}
	names := make(map[string]bool, len(t.Names))
	for n, ok := range t.Names {
		if ok {
			names[n] = true
		}
	}
	return Tools{Names: names}
}

// Connectors lists the connectors g names, sorted.
func (g Grant) Connectors() []string {
	out := make([]string, 0, len(g))
	for c := range g {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// String writes g as ParseGrant reads it, sorted.
func (g Grant) String() string {
	var parts []string
	for _, c := range g.Connectors() {
		t := g[c]
		if t.All {
			parts = append(parts, c)
			continue
		}
		names := make([]string, 0, len(t.Names))
		for n := range t.Names {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			parts = append(parts, c+"/"+n)
		}
	}
	return strings.Join(parts, " ")
}
