package pirn_test

import (
	"errors"
	"math/rand"
	"testing"

	"github.com/heedle-ai/pirn"
)

var (
	someConnectors = []string{"linear", "github", "files"}
	someTools      = []string{"search", "read", "create_issue"}
)

// grantFrom builds a grant from bytes: each byte names a connector and a
// tool, or a whole connector.
func grantFrom(b []byte) pirn.Grant {
	var args []string
	for _, x := range b {
		c := someConnectors[int(x)%len(someConnectors)]
		if t := int(x/3) % (len(someTools) + 1); t == len(someTools) {
			args = append(args, c)
		} else {
			args = append(args, c+"/"+someTools[t])
		}
	}
	g, err := pirn.ParseGrant(args)
	if err != nil {
		panic(err)
	}
	return g
}

// checkNarrowing checks the laws of Narrow on three grants: it allows a
// tool exactly when both sides do (so never more than either: a step only
// ever gets less), and it is commutative, associative and idempotent, so
// the order and repetition of caveats don't matter.
func checkNarrowing(t *testing.T, a, b, c pirn.Grant) {
	t.Helper()
	ab := a.Narrow(b)
	for _, conn := range someConnectors {
		for _, tool := range append(someTools, "other") {
			if ab.Allows(conn, tool) != (a.Allows(conn, tool) && b.Allows(conn, tool)) {
				t.Fatalf("%s/%s: %q narrowed by %q gives %q", conn, tool, a, b, ab)
			}
		}
	}
	if ab.String() != b.Narrow(a).String() {
		t.Fatalf("not commutative: %q, %q", a, b)
	}
	if a.Narrow(b).Narrow(c).String() != a.Narrow(b.Narrow(c)).String() {
		t.Fatalf("not associative: %q, %q, %q", a, b, c)
	}
	if a.Narrow(a).String() != a.String() {
		t.Fatalf("not idempotent: %q", a)
	}
}

// TestLaw_GrantsOnlyNarrow: the narrowing laws hold over many random
// grants.
func TestLaw_GrantsOnlyNarrow(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	bytes := func() []byte {
		b := make([]byte, r.Intn(6))
		r.Read(b)
		return b
	}
	for i := 0; i < 2000; i++ {
		checkNarrowing(t, grantFrom(bytes()), grantFrom(bytes()), grantFrom(bytes()))
	}
}

// FuzzGrantNarrowing: the same laws, on grants the fuzzer finds.
func FuzzGrantNarrowing(f *testing.F) {
	f.Add([]byte{0}, []byte{9}, []byte{})
	f.Add([]byte{0, 3, 6}, []byte{0, 1}, []byte{9, 10, 11})
	f.Fuzz(func(t *testing.T, a, b, c []byte) {
		checkNarrowing(t, grantFrom(a), grantFrom(b), grantFrom(c))
	})
}

// TestInv_GrantsAreParsedStrictly: a grant names connectors in lower
// case, with tools after a "/"; anything else is refused before a step
// starts; String writes what ParseGrant reads.
func TestInv_GrantsAreParsedStrictly(t *testing.T) {
	g, err := pirn.ParseGrant([]string{"linear/search_issues", "github", "github/x", "linear/get.issue"})
	if err != nil || g.String() != "github linear/get.issue linear/search_issues" {
		t.Errorf("grant %q, %v", g, err)
	}
	if again, _ := pirn.ParseGrant([]string{"github", "linear/get.issue", "linear/search_issues"}); again.String() != g.String() {
		t.Errorf("round trip %q", again)
	}
	for _, bad := range []string{"", "Linear", "linear/", "/x", "a b", "linear/x/y", "linear/" + string(make([]byte, 65)), "-x", "x__y", "linear/a b"} {
		if _, err := pirn.ParseGrant([]string{bad}); !errors.Is(err, pirn.ErrInvalid) {
			t.Errorf("accepted %q", bad)
		}
	}
}
