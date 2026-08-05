// Package stackcheck implements the nosplit frame budget: the check that a
// chain of functions which emit no stack-growth guard cannot, between them,
// use more stack than the runtime reserves below g.stackguard0.
//
// A function compiled with a stack-growth guard proves, at entry, that its own
// frame fits below the guard. A nosplit function proves nothing -- it subtracts
// from SP unconditionally. The runtime makes that safe by keeping a fixed
// reserve below the guard, so that any run of nosplit frames entered from a
// guarded function fits. Nothing enforces the "fits" part except a check like
// this one, and when it is missing the failure is a stack overflow in the
// runtime -- reported as "fatal error: runtime: split stack overflow", far from
// the compiler decision that caused it, and only on the executions that happen
// to enter the deepest chain.
//
// Go's linker performs this check in cmd/link/internal/ld/stackcheck.go, over
// the symbol graph, using each function's pcsp table for its stack height and
// its relocations for its call edges. cg12 has no equivalent link stage over Go
// code, so the check runs in the backend, where every frame has just been laid
// out and every call is still attached to the IR that produced it. The walk
// below is deliberately the same walk: heights accumulate bottom-up, a
// splittable callee ends a chain, an unresolved callee ends a chain, and a cycle
// is infinite.
//
// The one thing this package will not do is proceed. A budget that warns, or
// that truncates a chain it cannot measure, reintroduces exactly the runtime
// failure it exists to prevent, with a friendlier face on it: the build goes
// green and the program dies later. [Check] returns an error and the caller
// fails the build.
package stackcheck

import (
	"fmt"
	"sort"
	"strings"
)

// Func is one function as the budget sees it. The backend fills this in from a
// laid-out frame and the lowered IR; an assembly translator fills it in from a
// TEXT directive and the branches in the body.
type Func struct {
	// Name is the linker symbol. Call edges name their targets this way, so the
	// two must agree on spelling.
	Name string

	// Frame is the number of bytes this function subtracts from SP, including
	// its saved frame record, its spill area, its stack objects, and any fixed
	// outgoing-argument area. It is the height the chain gains by entering this
	// function.
	Frame int

	// NoSplit reports that the function emits no stack-growth guard. A
	// splittable function ends a chain: whatever it needs, it has proved it has.
	NoSplit bool

	// DynamicAlloc reports a stack allocation whose size is not a compile-time
	// constant. Such a frame has no upper bound, so no chain through it can be
	// shown to fit -- see [Report.Unbounded].
	DynamicAlloc bool

	// Calls are the direct call targets, by symbol name. A name this build does
	// not define is a chain end (see [Report.External]).
	Calls []string

	// Indirect reports a call through a function pointer, an interface method,
	// or a closure. What the edge costs is [Config.StrictIndirect]'s decision.
	Indirect bool

	// Opaque reports that the body was not examined, so Calls and Indirect say
	// nothing about what it may reach. Set for a function whose frame is known
	// but whose instructions this build never sees.
	Opaque bool

	// Unchecked marks a function that is declared splittable -- so the walk ends
	// a chain at it, as it does at any splittable function -- but that this
	// toolchain does not actually emit a stack-growth check for. It exists to be
	// counted and reported ([Report.Unchecked]), because that combination is a
	// hole in the guarantee and the budget should not be the thing that hides it.
	Unchecked bool

	// AddressTaken reports that something in the program can hold this
	// function's address, which makes it a possible target of any indirect call.
	AddressTaken bool
}

// Config carries the target's arithmetic and the one policy decision the walk
// cannot make from the graph.
type Config struct {
	// Limit is the number of bytes a whole nosplit chain may occupy.
	Limit int

	// CallSize is what the call instruction itself pushes: zero on a link-register
	// machine, one word on a machine whose call writes the return address to the
	// stack.
	CallSize int

	// StrictIndirect resolves a call through a function pointer against every
	// address-taken nosplit function, instead of assuming the target checks its
	// own stack.
	//
	// A call through a function pointer, an interface method, or a closure has no
	// single target, and the budget has to decide what it costs. The default is
	// Go's decision: cmd/link's stack check gives an indirect call the height of a
	// call to morestack -- that is, it assumes the target is an ordinary function
	// that will check its own stack -- and the Go runtime is written to keep that
	// true.
	//
	// Resolving the edge against every function whose address the program can hold
	// is sound, and it is not useful on real runtime code. Measured on goc's
	// runtime: 53 nosplit functions call indirectly and 149 nosplit functions are
	// address-taken, so the closure of that relation runs through most of the
	// runtime and closes cycles no execution takes -- runtime.systemstack's
	// indirect call to its argument reaches runtime.atomicwb, which reaches
	// systemstack again. Every chain then measures as infinite and the budget
	// rejects everything, which is a budget that has stopped saying anything.
	//
	// So this is an audit mode, not a gate. What it is good for is
	// [Report.AddressTakenNoSplit]: the set the default policy assumes nothing
	// about, which is the set a reader should be able to see.
	StrictIndirect bool
}

// Infinite is the height recorded for a chain that cannot be bounded: a cycle
// among nosplit functions, or a nosplit frame with a dynamic stack allocation.
// It is a number rather than a flag so that the ordinary "over the limit"
// comparison catches it without a special case, and it is far enough above any
// real limit that no sum of real frames reaches it.
const Infinite = 1 << 30

// Report is the result of a walk that stayed inside the budget. It records what
// the walk had to assume, so a caller can print it and a reader can tell an
// enforced bound from an unexamined one.
type Report struct {
	// Deepest is the nosplit chain with the greatest height, and Height is that
	// height. Deepest is empty when the module has no nosplit function.
	Deepest []string
	Height  int

	// External lists the callees, sorted, that no input defined. Each ends a
	// chain, on the same assumption Go's linker makes for a symbol outside the
	// link: that it is an ordinary function which will check its own stack.
	External []string

	// IndirectFromNoSplit lists the nosplit functions, sorted, that call through
	// a pointer. Under the default policy each such edge was assumed to reach a
	// function that checks its own stack; under Config.StrictIndirect each was
	// resolved against AddressTakenNoSplit instead. Either way the list is the
	// honest account of where the walk stopped seeing the program.
	IndirectFromNoSplit []string

	// AddressTakenNoSplit lists the nosplit functions, sorted, whose address the
	// program can hold. It is the set an indirect edge could actually enter
	// without a stack check, and so the set the default policy is assuming
	// nothing about. When it is empty, that assumption is a theorem.
	AddressTakenNoSplit []string

	// Opaque lists the nosplit functions, sorted, whose bodies were not
	// examined. Their frames are counted; what they call is not known.
	Opaque []string

	// Unchecked lists the functions, sorted, that ended a chain because they are
	// declared splittable but that emit no stack-growth check. Each is a place
	// the budget stops measuring on a promise the toolchain is not keeping.
	Unchecked []string
}

// Error is a nosplit chain that does not fit. Its message names the chain, each
// frame in it, the total, and the limit, because the useful question after this
// failure is which function to shrink, and that is not answerable from a total.
type Error struct {
	Config Config

	// Chains are the over-limit chains, deepest first.
	Chains []Chain
}

// Chain is one path through the nosplit call graph, from a function no nosplit
// function calls down to wherever the walk stopped.
type Chain struct {
	// Frames are the functions along the path, in call order.
	Frames []Frame

	// Height is the total stack the path occupies.
	Height int

	// Unbounded reports that the path cannot be given a height at all: it either
	// re-enters a function already on it, or passes through a frame with a
	// dynamic stack allocation. Height is [Infinite] in that case.
	Unbounded bool

	// Cycle names the function the path re-entered, when Unbounded is because of
	// recursion.
	Cycle string
}

// Frame is one function's contribution to a chain.
type Frame struct {
	Name string
	Size int

	// Dynamic reports that this frame also allocates a non-constant amount.
	Dynamic bool

	// Indirect reports that the edge *into* this frame was an indirect call
	// resolved against the address-taken set, rather than a direct call.
	Indirect bool
}

func (e *Error) Error() string {
	var out strings.Builder
	for index, chain := range e.Chains {
		if index > 0 {
			out.WriteString("\n")
		}
		out.WriteString(chain.describe(e.Config.Limit))
	}
	if len(e.Chains) > 1 {
		fmt.Fprintf(&out, "\n%d nosplit chains are over the %d-byte limit", len(e.Chains), e.Config.Limit)
	}
	return out.String()
}

func (c Chain) describe(limit int) string {
	var out strings.Builder
	head := "nosplit stack overflow"
	if c.Unbounded {
		head = "unbounded nosplit stack"
	}
	fmt.Fprintf(&out, "%s: %s", head, c.Frames[0].Name)
	for _, frame := range c.Frames[1:] {
		out.WriteString(" -> ")
		out.WriteString(frame.Name)
	}
	if c.Unbounded {
		if c.Cycle != "" {
			fmt.Fprintf(&out, " -> %s (cycle)", c.Cycle)
		}
		out.WriteString("\n")
	} else {
		fmt.Fprintf(&out, "\n  %d bytes of nosplit frames against a %d-byte limit, %d over\n",
			c.Height, limit, c.Height-limit)
	}
	for _, frame := range c.Frames {
		note := ""
		if frame.Dynamic {
			note = "  (dynamic stack allocation: unbounded)"
		}
		if frame.Indirect {
			note += "  (reached by an indirect call)"
		}
		fmt.Fprintf(&out, "  %8d  %s%s\n", frame.Size, frame.Name, note)
	}
	switch {
	case c.Cycle != "":
		fmt.Fprintf(&out, "  a nosplit function cannot call itself, directly or through another nosplit function\n")
	case c.Unbounded:
		fmt.Fprintf(&out, "  a nosplit function cannot allocate a variable amount of stack\n")
	default:
		fmt.Fprintf(&out, "  shrink a frame on this chain, or let one of these functions keep its stack-growth check\n")
	}
	return out.String()
}

// indirectTarget is the name of the synthetic node every indirect call from a
// nosplit function points at. Its callees are the address-taken nosplit
// functions, which is the set an indirect call could actually enter without a
// stack check; when that set is empty the node has height zero and the edge
// costs nothing, which is the same conclusion Go's linker reaches by assuming
// the target is splittable, reached here by proof instead of by assumption.
//
// The leading character cannot appear in a symbol name, so the node cannot
// collide with a real function.
const indirectTarget = "\x00indirect"

type walker struct {
	config Config
	funcs  map[string]*Func

	height map[string]int
	onPath map[string]bool

	external  map[string]bool
	opaque    map[string]bool
	indirect  map[string]bool
	unchecked map[string]bool

	addressTaken []string
}

// Check walks the nosplit call graph over funcs and reports whether every chain
// fits within config.Limit.
//
// It returns a non-nil *[Error] when any chain is over, and a *[Report] whenever
// the walk completes -- including alongside an error, so a caller can print what
// the walk assumed next to what it rejected.
func Check(funcs []Func, config Config) (*Report, error) {
	w := &walker{
		config:    config,
		funcs:     make(map[string]*Func, len(funcs)),
		height:    make(map[string]int, len(funcs)),
		onPath:    make(map[string]bool, len(funcs)),
		external:  map[string]bool{},
		opaque:    map[string]bool{},
		indirect:  map[string]bool{},
		unchecked: map[string]bool{},
	}
	// A duplicate definition keeps the larger frame. Two objects can legitimately
	// define the same symbol (an ABI0 alias, a weak definition), and the budget's
	// job is a worst case, not an inventory.
	for index := range funcs {
		f := &funcs[index]
		if existing, ok := w.funcs[f.Name]; ok {
			if f.Frame <= existing.Frame {
				continue
			}
		}
		w.funcs[f.Name] = f
	}
	for _, f := range w.funcs {
		if f.AddressTaken && f.NoSplit {
			w.addressTaken = append(w.addressTaken, f.Name)
		}
	}
	sort.Strings(w.addressTaken)

	// Heights bottom-up, memoized: every function is visited once, and a
	// function already on the current path is a cycle.
	names := make([]string, 0, len(w.funcs))
	for name := range w.funcs {
		names = append(names, name)
	}
	sort.Strings(names)

	var over []string
	for _, name := range names {
		if w.walk(name) > config.Limit {
			over = append(over, name)
		}
	}

	report := &Report{
		External:            sortedKeys(w.external),
		IndirectFromNoSplit: sortedKeys(w.indirect),
		AddressTakenNoSplit: w.addressTaken,
		Opaque:              sortedKeys(w.opaque),
		Unchecked:           sortedKeys(w.unchecked),
	}
	for _, name := range names {
		if !w.funcs[name].NoSplit {
			continue
		}
		if h := w.height[name]; h > report.Height {
			report.Height = h
			report.Deepest = w.trace(name)
		}
	}

	if len(over) == 0 {
		return report, nil
	}

	// Something is over. Report the chains from their roots -- the functions no
	// other nosplit function calls -- so the reader sees the whole path rather
	// than an arbitrary interior function of it. An over-limit function that is
	// only ever reached from another over-limit function is not reported
	// separately; its chain already contains it.
	roots := w.rootsOf(over)
	failure := &Error{Config: config}
	for _, root := range roots {
		failure.Chains = append(failure.Chains, w.chainFrom(root))
	}
	sort.SliceStable(failure.Chains, func(left, right int) bool {
		return failure.Chains[left].Height > failure.Chains[right].Height
	})
	return report, failure
}

// walk returns the stack height of name: the most stack that entering it can
// consume before something checks the stack again.
func (w *walker) walk(name string) int {
	if h, ok := w.height[name]; ok {
		return h
	}
	if w.onPath[name] {
		// Back edge. Record the cycle as infinite without memoizing it: the
		// height of the *other* functions on this cycle is not yet known, and
		// caching a value computed from a sentinel would leak it to callers that
		// are not part of the cycle.
		return Infinite
	}
	w.onPath[name] = true
	height := w.compute(name)
	delete(w.onPath, name)
	if height > Infinite {
		height = Infinite
	}
	w.height[name] = height
	return height
}

func (w *walker) compute(name string) int {
	if name == indirectTarget {
		// An indirect call can enter any function whose address the program can
		// hold. Those that are splittable check their own stack and end the
		// chain; those that are nosplit do not, and are what this node stands
		// for.
		height := 0
		for _, target := range w.addressTaken {
			if h := w.walk(target); h > height {
				height = h
			}
		}
		return height
	}
	f, ok := w.funcs[name]
	if !ok {
		// Nothing in this build defines the symbol. Go's linker makes the same
		// call for a symbol outside the link (ld.stackCheck.computeHeight
		// returns 0 for an external symbol): assume it is an ordinary function
		// that checks its own stack. Recorded, so the assumption is visible.
		w.external[name] = true
		return 0
	}
	if !f.NoSplit {
		// A splittable function has proved at entry that its own frame fits
		// below the guard, and every chain below it starts over from there. On a
		// machine whose call pushes the return address, the push happens before
		// that proof, so it is charged here.
		if f.Unchecked {
			w.unchecked[name] = true
		}
		return w.config.CallSize
	}
	if f.DynamicAlloc {
		return Infinite
	}
	// The function is nosplit: its frame is spent unconditionally, and so is the
	// frame of anything it goes on to call without a check in between.
	height := f.Frame
	if f.Opaque {
		w.opaque[name] = true
	}
	for _, callee := range f.Calls {
		if h := f.Frame + w.config.CallSize + w.walk(callee); h > height {
			height = h
		}
	}
	if f.Indirect {
		w.indirect[name] = true
		// Under the default policy the target checks its own stack, so the edge
		// costs this frame plus the call and nothing below it.
		edge := f.Frame + w.config.CallSize
		if w.config.StrictIndirect {
			edge += w.walk(indirectTarget)
		}
		if edge > height {
			height = edge
		}
	}
	return height
}

// rootsOf reduces a set of over-limit functions to the ones worth reporting: a
// function that some other over-limit function already reaches is dropped,
// because the surviving chain passes through it and names it. A cycle whose
// members all reach each other keeps its lowest-sorting member, so a mutually
// recursive pair is reported once rather than never.
func (w *walker) rootsOf(over []string) []string {
	inOver := make(map[string]bool, len(over))
	for _, name := range over {
		inOver[name] = true
	}
	// Reachability is computed once per origin with its own visited set, seeded
	// with the origin itself. A path that comes back around to where it started
	// therefore does not mark its own origin, so a recursive over-limit function
	// is still its own root and still gets reported.
	reachedFromElsewhere := map[string]bool{}
	for _, origin := range over {
		visited := map[string]bool{origin: true}
		stack := []string{origin}
		for len(stack) > 0 {
			name := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, callee := range w.calleesOf(name) {
				// Stop at the boundary of the over-limit set: a function whose
				// own chain fits is not a candidate for reporting.
				if !inOver[callee] || visited[callee] {
					continue
				}
				visited[callee] = true
				reachedFromElsewhere[callee] = true
				stack = append(stack, callee)
			}
		}
	}
	var roots []string
	for _, name := range over {
		if !reachedFromElsewhere[name] {
			roots = append(roots, name)
		}
	}
	if len(roots) == 0 && len(over) > 0 {
		// Every over-limit function is reachable from another: the set is one or
		// more cycles with no entry point among them. Keep the first, which is
		// the lowest-sorting name because `over` was built in sorted order.
		roots = over[:1]
	}
	return roots
}

func (w *walker) calleesOf(name string) []string {
	if name == indirectTarget {
		return w.addressTaken
	}
	f, ok := w.funcs[name]
	if !ok || !f.NoSplit {
		return nil
	}
	if !f.Indirect || !w.config.StrictIndirect {
		return f.Calls
	}
	return append(append([]string(nil), f.Calls...), indirectTarget)
}

// chainFrom rebuilds the deepest path out of root as a printable chain. It
// re-walks rather than reading memoized heights so that it can name the cycle it
// hits, which a height cannot express.
func (w *walker) chainFrom(root string) Chain {
	var chain Chain
	onPath := map[string]bool{}
	name := root
	indirectEdge := false
	for {
		if name == indirectTarget {
			// Fold the synthetic node out of the printed chain; the frame that
			// follows carries the note instead.
			indirectEdge = true
			next, cycle := w.deepestCallee(name, onPath)
			if cycle != "" {
				chain.Unbounded = true
				chain.Cycle = cycle
				chain.Height = Infinite
				return chain
			}
			if next == "" {
				return w.finishChain(chain)
			}
			name = next
			continue
		}
		f, ok := w.funcs[name]
		if !ok || !f.NoSplit {
			break
		}
		if onPath[name] {
			chain.Unbounded = true
			chain.Cycle = name
			chain.Height = Infinite
			return chain
		}
		onPath[name] = true
		chain.Frames = append(chain.Frames, Frame{
			Name:     name,
			Size:     f.Frame,
			Dynamic:  f.DynamicAlloc,
			Indirect: indirectEdge,
		})
		chain.Height += f.Frame + w.config.CallSize
		indirectEdge = false
		if f.DynamicAlloc {
			chain.Unbounded = true
			chain.Height = Infinite
			return chain
		}
		next, cycle := w.deepestCallee(name, onPath)
		if cycle != "" {
			chain.Unbounded = true
			chain.Cycle = cycle
			chain.Height = Infinite
			return chain
		}
		if next == "" {
			break
		}
		name = next
	}
	return w.finishChain(chain)
}

// finishChain removes the trailing call-size charge the loop added for a call
// that turned out not to happen: the last frame on the chain is a leaf, or its
// deepest callee is splittable and pays its own way.
func (w *walker) finishChain(chain Chain) Chain {
	if len(chain.Frames) > 0 {
		chain.Height -= w.config.CallSize
	}
	return chain
}

// deepestCallee picks the callee that accounts for name's height, which is the
// edge the chain should follow. It returns a cycle name instead when the deepest
// callee is already on the path.
func (w *walker) deepestCallee(name string, onPath map[string]bool) (next, cycle string) {
	best := -1
	for _, callee := range w.calleesOf(name) {
		if onPath[callee] {
			return "", callee
		}
		if h := w.walk(callee); h > best {
			best = h
			next = callee
		}
	}
	if best <= 0 {
		// Every callee ends the chain (splittable, external, or a leaf). Follow
		// none of them: the frames below contribute nothing.
		return "", ""
	}
	return next, ""
}

// trace is chainFrom reduced to the names, for [Report.Deepest].
func (w *walker) trace(root string) []string {
	chain := w.chainFrom(root)
	names := make([]string, 0, len(chain.Frames))
	for _, frame := range chain.Frames {
		names = append(names, frame.Name)
	}
	if chain.Cycle != "" {
		names = append(names, chain.Cycle+" (cycle)")
	}
	return names
}

func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
