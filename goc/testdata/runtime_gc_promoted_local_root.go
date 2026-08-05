// A local pointer variable that mem2reg promotes out of its stack slot has to
// stay visible to the collector for as long as the slot would have been.
//
// A managed local lives in an alloca whose pointer word the frame map describes
// for the whole span the allocation reaches -- ir.InferStackPointerWords records
// the word, arm64's recordSafepoint emits it at every safepoint. The values that
// pass through the slot need no marking of their own, because between the call
// that produces one and the store that files it away nothing can collect. So a
// frontend can get away with marking a *load* from a managed slot as a GC
// reference and not the call result stored into it, and goc does: the result of
// a multi-result constructor like nistec.P256Point.SetBytes arrives unmarked.
//
// mem2reg deletes the slot and the loads, and then that unmarked value is what
// carries the variable across every safepoint in between. Before opt/mem2reg.go's
// markManagedDef it was reported at no safepoint at all, and the object was freed
// under a pointer the program was still going to use.
//
// This is that, reduced. `held` is filled by a constructor that also returns an
// error -- the multi-result return is what produces the unmarked temporary -- and
// is then held across six collections with nothing else referring to it. The
// finalizer is the detector rather than the corrupted contents: a freed object is
// not necessarily overwritten, so checking its fields catches this only about
// five runs in six, while a finalizer that runs while the program is still
// holding the pointer is the defect itself and fires every time.
//
// It found crypto/internal/fips140/ecdsa.verifyGeneric, whose `Q` is exactly this
// shape: goc-built goc/testdata/placement_bench/p256 printed "signature did not
// verify" on 35 runs in 40 at GOGC=10 with promotion on, and 0 with it off.
package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
)

// held is the object the promoted local points at. Its size class is shared with
// the garbage churn below, so a span released under a live pointer is handed
// straight back out.
type held struct {
	seed  int
	label string
	fill  [24]int
}

// finalized records, per seed, that the collector decided that seed's object was
// unreachable. The finalizer takes the object as its argument rather than
// capturing it, so it does not itself keep anything alive.
var finalized [16]bool

// make2 returns its result together with an error. The multi-result return is
// what puts the pointer in a temporary the frontend does not mark, which is the
// value the promoted variable then inherits.
//
//go:noinline
func make2(seed int) (*held, error) {
	if seed < 0 {
		return nil, fmt.Errorf("negative seed %d", seed)
	}
	h := &held{seed: seed, label: "held"}
	for i := range h.fill {
		h.fill[i] = seed*1000 + i
	}
	runtime.SetFinalizer(h, func(dead *held) { finalized[dead.seed] = true })
	return h, nil
}

// churn allocates and drops garbage of the same size class, so a span freed under
// a live pointer is reused and overwritten.
//
//go:noinline
func churn(rounds int) int {
	total := 0
	for i := 0; i < rounds; i++ {
		junk := &held{seed: -1, label: "junk"}
		for j := range junk.fill {
			junk.fill[j] = 0x5a5a5a5a
		}
		total += junk.fill[i%len(junk.fill)]
	}
	return total
}

//go:noinline
func check(seed int) error {
	h, err := make2(seed)
	if err != nil {
		return err
	}
	// h is live across every one of these collections and nothing else refers to
	// it, so it is reachable only through the promoted local.
	sink := 0
	for round := 0; round < 6; round++ {
		runtime.GC()
		sink += churn(64)
	}
	if h.seed != seed || h.label != "held" {
		return fmt.Errorf("object was recycled: seed=%d label=%q", h.seed, h.label)
	}
	for i := range h.fill {
		if h.fill[i] != seed*1000+i {
			return fmt.Errorf("object was recycled: fill[%d]=%d want %d", i, h.fill[i], seed*1000+i)
		}
	}
	if finalized[seed] {
		return fmt.Errorf("object was finalized while the program was still using it")
	}
	_ = sink
	return nil
}

func main() {
	debug.SetGCPercent(10)
	for seed := 1; seed <= 8; seed++ {
		if err := check(seed); err != nil {
			fmt.Println("promoted local lost:", err)
			os.Exit(1)
		}
	}
	fmt.Println("promoted local stayed visible")
}
