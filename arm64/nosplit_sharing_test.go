package arm64

import (
	"testing"

	"github.com/evanphx/cg12/stackcheck"
)

// The relation these tests pin down is what makes a sequence of independent
// per-function inlining decisions add up to a bound on a chain. Headroom is
// measured once for the whole module; if the pass then grows two functions that
// lie on one chain, the chain gains both growths while each decision saw only
// the original slack. noSplitChainSharing is the set that has to be charged for
// that not to happen, and it has to be the same set the walk itself would call a
// chain -- no wider, or the pass loses inlining it could afford, and no
// narrower, or the bound is not one.

func noSplitFunc(name string, frame int, calls ...string) stackcheck.Func {
	return stackcheck.Func{Name: name, Frame: frame, NoSplit: true, Calls: calls}
}

func splittableFunc(name string, frame int, calls ...string) stackcheck.Func {
	return stackcheck.Func{Name: name, Frame: frame, Calls: calls}
}

func sharedWith(t *testing.T, sharing map[string][]string, name string) map[string]bool {
	t.Helper()
	set := map[string]bool{}
	for _, other := range sharing[name] {
		set[other] = true
	}
	return set
}

func requireShares(t *testing.T, sharing map[string][]string, name string, want ...string) {
	t.Helper()
	got := sharedWith(t, sharing, name)
	if len(got) != len(want) {
		t.Fatalf("%s shares with %v, want %v", name, sharing[name], want)
	}
	for _, expected := range want {
		if !got[expected] {
			t.Fatalf("%s shares with %v, want %v", name, sharing[name], want)
		}
	}
}

// Everything on one chain shares it, in both directions: a function is on a
// chain with the callers above it as much as with the callees below it, because
// a chain does not care which end grew.
func TestChainSharingReachesUpAndDown(t *testing.T) {
	sharing := noSplitChainSharing([]stackcheck.Func{
		noSplitFunc("top", 64, "middle"),
		noSplitFunc("middle", 64, "bottom"),
		noSplitFunc("bottom", 64),
	})
	requireShares(t, sharing, "top", "top", "middle", "bottom")
	requireShares(t, sharing, "middle", "top", "middle", "bottom")
	requireShares(t, sharing, "bottom", "top", "middle", "bottom")
}

// A splittable function proves its own frame fits and the chain starts over
// below it, so it is not on the chain and neither is anything under it.
func TestChainSharingStopsAtASplittableFunction(t *testing.T) {
	sharing := noSplitChainSharing([]stackcheck.Func{
		noSplitFunc("top", 64, "guarded"),
		splittableFunc("guarded", 64, "bottom"),
		noSplitFunc("bottom", 64),
	})
	requireShares(t, sharing, "top", "top")
	requireShares(t, sharing, "bottom", "bottom")
	if _, ok := sharing["guarded"]; ok {
		t.Error("a splittable function was given a chain of its own")
	}
}

// Two chains that never meet must not charge each other, or the pass loses
// inlining it could afford everywhere in the module because of one growth
// somewhere in it.
func TestChainSharingKeepsSeparateChainsApart(t *testing.T) {
	sharing := noSplitChainSharing([]stackcheck.Func{
		noSplitFunc("leftTop", 64, "leftBottom"),
		noSplitFunc("leftBottom", 64),
		noSplitFunc("rightTop", 64, "rightBottom"),
		noSplitFunc("rightBottom", 64),
	})
	requireShares(t, sharing, "leftTop", "leftTop", "leftBottom")
	requireShares(t, sharing, "rightBottom", "rightTop", "rightBottom")
}

// A callee this build does not define ends the chain, exactly as the walk ends
// it: Go's linker assumes an external symbol checks its own stack.
func TestChainSharingIgnoresUndefinedCallees(t *testing.T) {
	sharing := noSplitChainSharing([]stackcheck.Func{
		noSplitFunc("top", 64, "somewhere_else"),
	})
	requireShares(t, sharing, "top", "top")
}

// A cycle among nosplit functions is unbounded to the walk, but the sharing
// relation still has to terminate and still has to name every member.
func TestChainSharingTerminatesOnACycle(t *testing.T) {
	sharing := noSplitChainSharing([]stackcheck.Func{
		noSplitFunc("first", 64, "second"),
		noSplitFunc("second", 64, "first"),
		noSplitFunc("selfish", 64, "selfish"),
	})
	requireShares(t, sharing, "first", "first", "second")
	requireShares(t, sharing, "second", "first", "second")
	requireShares(t, sharing, "selfish", "selfish")
}

// The budget's own arithmetic: charging one function takes the bytes out of
// everything on its chains and out of nothing else.
func TestChargeSpendsTheChainOnceAndOnlyItsOwnChain(t *testing.T) {
	funcs := []stackcheck.Func{
		noSplitFunc("top", 64, "bottom"),
		noSplitFunc("bottom", 64),
		noSplitFunc("elsewhere", 64),
	}
	budget := &noSplitFrameBudget{
		headroom: map[string]int{"top": 300, "bottom": 300, "elsewhere": 300},
		sharing:  noSplitChainSharing(funcs),
		charged:  map[string]int{},
	}

	budget.Charge("top", 100)

	if got := budget.Headroom("top"); got != 200 {
		t.Errorf("top headroom = %d, want 200", got)
	}
	if got := budget.Headroom("bottom"); got != 200 {
		t.Errorf("bottom headroom = %d, want 200 -- the growth above it is on its chain", got)
	}
	if got := budget.Headroom("elsewhere"); got != 300 {
		t.Errorf("elsewhere headroom = %d, want 300 -- it is on no chain with top", got)
	}

	// A shrink is not credited back. The headroom map was measured before any of
	// this, so handing out bytes it never counted would be the same mistake in
	// the other direction.
	budget.Charge("top", -100)
	if got := budget.Headroom("top"); got != 200 {
		t.Errorf("top headroom = %d after a negative charge, want 200", got)
	}
}

// A function the walk never saw is offered nothing, charged or not.
func TestChargeLeavesAnUnmeasuredFunctionWithNothing(t *testing.T) {
	budget := &noSplitFrameBudget{
		headroom: map[string]int{},
		sharing:  map[string][]string{},
		charged:  map[string]int{},
	}
	if got := budget.Headroom("never_measured"); got != 0 {
		t.Errorf("headroom = %d for a function the walk did not see, want 0", got)
	}
}
