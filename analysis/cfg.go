package analysis

import (
	"sync"

	"github.com/evanphx/cg12/ir"
)

// CFG holds control-flow information derived from a function: predecessor lists
// (written back onto the blocks) and a reverse-postorder numbering. It is the
// substrate for dominance and liveness.
type CFG struct {
	Fn  *ir.Func
	RPO []*ir.Block // reverse postorder; RPO[0] is the entry block

	// num maps a block to its index within RPO. int32 rather than int because a
	// map's value width is paid for every slot of every bucket it allocates, and
	// this map is built once per function per pass per fixpoint round -- it was
	// 93 MB on a -O compile of the SQLite amalgamation, the largest single
	// allocation the CFG makes.
	num map[*ir.Block]int32

	// released marks a CFG whose numbering has been handed back; see [CFG.Release].
	released bool
}

// BuildCFG computes predecessors and reverse postorder for f. Blocks not
// reachable from the entry are omitted from RPO (and thus from later analyses).
//
// A caller that is finished with the result should say so with [CFG.Release],
// which hands the numbering map back to be filled in again rather than
// collected. See there for why that is worth doing and when it is not allowed.
func BuildCFG(f *ir.Func) *CFG {
	fillPreds(f)
	c := &CFG{Fn: f, num: takeNumbering(len(f.Blocks))}
	c.computeRPO()
	return c
}

// pooledNumberingLimit bounds the size of a numbering map worth recycling.
//
// A map handed back from a large function keeps its buckets for as long as the
// pool holds it, and would then be handed to a small one that cannot use them.
// Measured on a -O compile of the SQLite amalgamation, half of all CFG builds
// are on functions of sixteen blocks or fewer and 96% on 128 or fewer, so a
// modest limit captures nearly all of the reuse and none of the retention.
const pooledNumberingLimit = 256

var numberingPool sync.Pool

// takeNumbering returns a map to number size blocks into, recycled if one is
// waiting. A function too large to have been pooled gets a fresh presized map:
// growing a small recycled one into place would rehash all the way up, which is
// the cost presizing exists to avoid.
func takeNumbering(size int) map[*ir.Block]int32 {
	if size <= pooledNumberingLimit {
		if recycled, ok := numberingPool.Get().(map[*ir.Block]int32); ok {
			return recycled
		}
	}
	return make(map[*ir.Block]int32, size)
}

// Release says the caller is done with c, so its numbering map can be filled in
// again by the next build instead of being collected.
//
// It is worth saying: a compile builds a CFG per function per pass per fixpoint
// round -- 60,629 of them for the SQLite amalgamation -- and the map is the
// largest thing each one allocates, at 29 bytes for every block. Nothing else
// about the CFG is reused; only the storage.
//
// The rule is that c must be dead. Releasing a CFG something still holds would
// let the next build overwrite the answers it gives, which is why reading a
// released CFG panics rather than quietly returning a number from somebody
// else's function. Do not release a CFG that is returned, stored in a struct
// that outlives the call, or captured by a closure that does -- and when in
// doubt, do not release at all: the cost of not releasing is the allocation
// this saves, which is the situation before it existed.
func (c *CFG) Release() {
	if c == nil || c.released {
		return
	}
	c.released = true
	numbering := c.num
	c.num = nil
	// RPO is deliberately left alone. It is not recycled -- nothing else will
	// write to that slice -- so it stays correct, and clearing it would be a trap
	// rather than a check: RPO is a plain exported field, so a read after release
	// cannot be intercepted, and a nil one would quietly turn a pass that walks it
	// into a pass that does nothing. Only the map is handed back, and only the map
	// reads are guarded.
	if len(numbering) > pooledNumberingLimit {
		return
	}
	clear(numbering)
	numberingPool.Put(numbering)
}

// checkLive panics when a released CFG is read. The alternative is worse than a
// panic: c.num is nil after a release, and a nil map reads as "no such block",
// so every block would silently report itself unreachable.
func (c *CFG) checkLive() {
	if c.released {
		panic("analysis: CFG read after Release")
	}
}

// Succs returns the executable and synthetic analysis successors of b.
func (c *CFG) Succs(b *ir.Block) []*ir.Block {
	successors := make([]*ir.Block, 0, b.SuccCount()+len(b.SyntheticSuccs))
	for i := 0; i < b.SuccCount(); i++ {
		successors = append(successors, b.SuccAt(i))
	}
	successors = append(successors, b.SyntheticSuccs...)
	return successors
}

// succCount and succAt walk the same sequence [CFG.Succs] returns -- a block's
// executable successors followed by its synthetic ones -- without building the
// slice. The CFG walk runs once per block per pass per fixpoint round, so the
// slice it used to build was pure garbage at compile-time scale.
func succCount(b *ir.Block) int {
	return b.SuccCount() + len(b.SyntheticSuccs)
}

func succAt(b *ir.Block, i int) *ir.Block {
	if executable := b.SuccCount(); i < executable {
		return b.SuccAt(i)
	} else {
		return b.SyntheticSuccs[i-executable]
	}
}

// FillPreds rebuilds each block's predecessor list and nothing else.
//
// It is [BuildCFG] without the reverse postorder, for a caller that mutates the
// graph and needs the predecessor lists to catch up but never asks a block for
// its RPO number. That is the whole of what BuildCFG does to f -- computeRPO
// writes only into the CFG it returns -- so calling this instead is the same
// change to the function with none of the numbering.
//
// It exists because SimplifyCFG's block coalescing called BuildCFG after every
// single fusion, purely to refresh Preds, and each of those built and threw away
// a block-keyed map, a postorder slice and a DFS stack. Coalescing was 40% of
// the 107,026 CFG builds a -O compile of the SQLite amalgamation performs.
func FillPreds(f *ir.Func) { fillPreds(f) }

// fillPreds rebuilds each block's predecessor list from the executable and
// synthetic control-flow edges. A predecessor is listed once even if several
// edges target the same block.
func fillPreds(f *ir.Func) {
	for _, b := range f.Blocks {
		b.Preds = b.Preds[:0]
	}
	for _, b := range f.Blocks {
		for i, count := 0, succCount(b); i < count; i++ {
			s := succAt(b, i)
			if s == nil {
				continue
			}
			// The duplicate this drops is a second edge from b to s -- a `jnz c,
			// @x, @x`, or two switch cases landing on one block. Only b appends to
			// any predecessor list while b's own successors are being walked, so b
			// has already been recorded for s exactly when it is the last entry in
			// s.Preds. That is the whole of the set the loop used to carry.
			if len(s.Preds) > 0 && s.Preds[len(s.Preds)-1] == b {
				continue
			}
			s.Preds = append(s.Preds, b)
		}
	}
}

// rpoFrame is one entry of computeRPO's explicit DFS stack: the block being
// visited and how far through its successors the walk has got.
type rpoFrame struct {
	block *ir.Block
	next  int
}

func (c *CFG) computeRPO() {
	// The walk is the recursive postorder DFS it has always been, written with an
	// explicit stack: the closure the recursive form needed escaped to the heap on
	// every call, and so did the separate visited set. c.num doubles as that set,
	// holding visiting until the reverse pass overwrites it with the real index.
	const visiting = -1
	post := make([]*ir.Block, 0, len(c.Fn.Blocks))
	if c.Fn.Start != nil {
		stack := make([]rpoFrame, 0, len(c.Fn.Blocks))
		stack = append(stack, rpoFrame{block: c.Fn.Start})
		c.num[c.Fn.Start] = visiting
		for len(stack) > 0 {
			top := len(stack) - 1
			b := stack[top].block
			descended := false
			for count := succCount(b); stack[top].next < count; {
				s := succAt(b, stack[top].next)
				stack[top].next++
				if s == nil {
					continue
				}
				if _, seen := c.num[s]; seen {
					continue
				}
				c.num[s] = visiting
				stack = append(stack, rpoFrame{block: s})
				descended = true
				break
			}
			if descended {
				continue
			}
			post = append(post, b)
			stack = stack[:top]
		}
	}
	// Reverse the postorder in place and keep it as the RPO, rather than copying
	// it into a second slice of the same size: the copy was 26 MB of a -O compile
	// of the SQLite amalgamation, holding exactly what the first slice already
	// held, backwards.
	for i, j := 0, len(post)-1; i < j; i, j = i+1, j-1 {
		post[i], post[j] = post[j], post[i]
	}
	c.RPO = post
	for i, b := range c.RPO {
		c.num[b] = int32(i)
	}
}

// Num returns the reverse-postorder index of b, or -1 if b is unreachable.
func (c *CFG) Num(b *ir.Block) int {
	c.checkLive()
	if n, ok := c.num[b]; ok {
		return int(n)
	}
	return -1
}

// Reachable reports whether b is reachable from the entry.
func (c *CFG) Reachable(b *ir.Block) bool {
	c.checkLive()
	_, ok := c.num[b]
	return ok
}
