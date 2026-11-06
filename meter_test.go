package pirn_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/heedle-ai/bobbin"
	"github.com/heedle-ai/pirn"
	"github.com/heedle-ai/shuttle/plan"
)

// TestInv_EveryCallIsMetered: every request Claude answers is noted on
// the context's Meter the moment it answers, an ask's and each agent
// turn's alike, even when the step then fails: here the second turn is
// refused, and the first, which Claude answered, is still counted; a
// refused request is not (PIRN-320, roadmap 10.4.1).
func TestInv_EveryCallIsMetered(t *testing.T) {
	ctx := context.Background()
	ask := newScript(t, turn("end_turn", `{"type":"text","text":"hi"}`))
	m := &pirn.Meter{}
	if _, err := (pirn.Claude{APIKey: "k", BaseURL: ask.srv.URL}).Ask(pirn.WithMeter(ctx, m), pirn.Question{Instruction: "Say hi."}); err != nil {
		t.Fatal(err)
	}
	if got := m.Calls(); !reflect.DeepEqual(got, []pirn.Call{{Model: "claude-sonnet-5-5", Input: 10, Output: 3}}) {
		t.Errorf("an ask's calls: %+v", got)
	}

	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if n.Add(1) == 1 {
			io.WriteString(w, turn("tool_use", toolUse("t1", "ls")))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`)
	}))
	t.Cleanup(srv.Close)
	podman, _ := fakePodman(t, "ok")
	a := pirn.Agent{Claude: pirn.Claude{APIKey: "k", BaseURL: srv.URL}, Runner: bobbin.Runner{Podman: podman}, Image: zeros}
	m = &pirn.Meter{}
	if _, err := a.Run(pirn.WithMeter(ctx, m), pirn.Question{Instruction: "List."}); err == nil {
		t.Fatal("the agent step should fail on the refused turn")
	}
	if got := m.Calls(); !reflect.DeepEqual(got, []pirn.Call{{Model: "claude-sonnet-5-5", Input: 10, Output: 3}}) {
		t.Errorf("a failed agent step's calls: %+v", got)
	}
	// An act step's turns too (review 56).
	act, _ := newAct(t, newActAPI(t, actTurn("end_turn", `{"type":"text","text":"Nothing to do.\nResult: finished"}`)))
	m = &pirn.Meter{}
	if _, err := pirn.ActCommand(act)(pirn.WithMeter(ctx, m), plan.Input{NodeID: "n3", Args: []string{"Sign up.", "https://forms.example.org/"}}); err != nil {
		t.Fatal(err)
	}
	if got := m.Calls(); len(got) != 1 || got[0].Input == 0 {
		t.Errorf("an act step's calls: %+v", got)
	}

	// Every request pirn makes is metered where Claude answers it.
	if sites := meteredSites(t); sites < 2 {
		t.Errorf("found %d requests to Claude; pirn makes at least an ask's and an agent turn's", sites)
	}
	var none *pirn.Meter
	none.Add(pirn.Call{Model: "x"}) // a nil Meter counts nothing, and doesn't panic
	if none.Calls() != nil || pirn.MeterFrom(ctx) != nil {
		t.Error("no meter should count nothing")
	}
}

// meteredSites checks, in every Go file of pirn but tests (any folder),
// that each request to Claude (a call of the SDK's Messages.New, or any
// Messages.New... such as NewStreaming) assigns its message and is
// followed, in the same block, by MeterFrom(...).Add(...): as its own
// statement, or in an "if err == nil" right after, never only in an error
// branch (review 56 round 2). It returns how many it found.
func meteredSites(t *testing.T) int {
	t.Helper()
	isRequest := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !strings.HasPrefix(sel.Sel.Name, "New") {
			return false
		}
		recv, ok := sel.X.(*ast.SelectorExpr)
		return ok && recv.Sel.Name == "Messages"
	}
	isAdd := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Add" {
					if inner, ok := sel.X.(*ast.CallExpr); ok {
						if id, ok := inner.Fun.(*ast.Ident); ok && id.Name == "MeterFrom" {
							found = true
						}
					}
				}
			}
			return !found
		})
		return found
	}
	isErrNil := func(e ast.Expr) bool {
		b, ok := e.(*ast.BinaryExpr)
		if !ok || b.Op != token.EQL {
			return false
		}
		x, ok1 := b.X.(*ast.Ident)
		y, ok2 := b.Y.(*ast.Ident)
		return ok1 && ok2 && x.Name == "err" && y.Name == "nil"
	}
	sites := 0
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && path != "." {
			if err == nil {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		requests := 0
		ast.Inspect(f, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && isRequest(c) {
				requests++
			}
			return true
		})
		metered := 0
		ast.Inspect(f, func(n ast.Node) bool {
			b, ok := n.(*ast.BlockStmt)
			if !ok {
				return true
			}
			for i, st := range b.List {
				as, ok := st.(*ast.AssignStmt)
				if !ok || len(as.Rhs) != 1 || !isRequest(as.Rhs[0]) {
					continue
				}
				for _, next := range b.List[i+1:] {
					if ex, ok := next.(*ast.ExprStmt); ok && isAdd(ex) {
						metered++
						break
					}
					if is, ok := next.(*ast.IfStmt); ok && isErrNil(is.Cond) && isAdd(is.Body) {
						metered++
						break
					}
				}
			}
			return true
		})
		sites += requests
		if metered != requests {
			t.Errorf("%s: %d requests to Claude, %d of them metered where Claude answers", path, requests, metered)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sites
}
