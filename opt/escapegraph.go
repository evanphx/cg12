package opt

import (
	"strings"

	"github.com/evanphx/cg12/ir"
)

// The per-function half of the summary analysis: a weighted location graph over
// one ir.Func, solved the way cmd/compile/internal/escape solves its own.
//
// A location is a place a value can be held: this function's parameters and SSA
// temporaries, each frame slot the function writes through, one node per result,
// and one node for the heap. An edge dst <- src with weight k means "the value
// obtained by dereferencing src k times is assigned to dst": 0 for a copy, +1
// for a load, -1 for an address.
//
// The weight is the whole point. A boolean "does this parameter escape" cannot
// express the difference between
//
//	func f(p **T) { sink = *p }   // the pointee escapes, p does not
//	func g(p *T)  { sink = p }    // p escapes
//
// and that difference is load-bearing: it is the same one addressedVariableIdentifier
// makes in goc/compile.go, where promoting a variable because &p.f escaped moves
// the pointer and leaves the pointee where it was. Here it decides whether an
// allocation passed to a callee has to be on the heap or whether only something
// reachable from it does.
//
// A location escapes when the heap node reaches it with a net dereference count
// below zero -- that is, when its *address* flows to the heap. A parameter is
// summarised by the smallest net count at which it is reached from the heap or
// from a result: zero means the pointer itself is retained, one or more means
// only what it points at is.

// escapeEdge is one weighted edge of the location graph.
type escapeEdge struct {
	src    int32
	derefs int32
}

// escapeUnreached is the initial distance in a walk. It is far enough above any
// real dereference count that the first relaxation always wins.
const escapeUnreached = int32(1 << 20)

// escapeHeapLoc is the location id of the heap node. Result nodes follow it,
// then temporaries, then frame slots.
const escapeHeapLoc = 0

type escapeGraph struct {
	function *ir.Func
	byName   map[string]*ir.Func
	facts    *EscapeFacts
	aliases  *aliasInfo

	results  int // number of result nodes
	tempBase int // location id of temporary 0
	regions  map[locKey]int

	edges   [][]escapeEdge
	escapes []bool
	// forced lists locations the analysis cannot reason about and must treat as
	// escaping outright. See closureEnvironment.
	forced []int

	// paramIndex maps a location id to a parameter index, for the locations
	// that are parameters.
	paramIndex map[int]int
	// resultParams maps a result-area parameter's temporary id to the result it
	// stands for, so a store through it is a leak to that result rather than a
	// publication.
	resultParams map[uint32]int

	// heapLeak[i] is the smallest dereference count at which parameter i is
	// reached from the heap; resultLeak[i] is the same per result index.
	heapLeak   []int32
	resultLeak []map[int]int32

	// definitions is the instruction that defines each temporary, for
	// scalarValue's walk back through copies and casts.
	definitions map[uint32]ir.Instr
}

func analyzeFunction(byName map[string]*ir.Func, function *ir.Func, facts *EscapeFacts) []ParamFact {
	graph := newEscapeGraph(byName, function, facts)
	graph.build()
	graph.solve()
	return graph.summary()
}

func newEscapeGraph(byName map[string]*ir.Func, function *ir.Func, facts *EscapeFacts) *escapeGraph {
	graph := &escapeGraph{
		function:     function,
		byName:       byName,
		facts:        facts,
		aliases:      newAliasInfo(function),
		results:      resultCount(function),
		regions:      make(map[locKey]int),
		paramIndex:   make(map[int]int, len(function.Params)),
		resultParams: make(map[uint32]int),
	}
	graph.tempBase = 1 + graph.results
	graph.edges = make([][]escapeEdge, graph.tempBase+len(function.Temps))
	graph.heapLeak = make([]int32, len(function.Params))
	graph.resultLeak = make([]map[int]int32, len(function.Params))
	for index := range graph.heapLeak {
		graph.heapLeak[index] = escapeUnreached
	}
	for index, parameter := range function.Params {
		if parameter == nil {
			continue
		}
		graph.paramIndex[graph.tempBase+parameter.ID] = index
	}
	next := 0
	for _, parameter := range function.Params {
		if parameter == nil || !strings.HasPrefix(parameter.Name, "result") {
			continue
		}
		graph.resultParams[uint32(parameter.ID)] = next
		next++
	}
	return graph
}

// resultCount is how many result nodes the function needs: one per scalar
// return value, or one for an aggregate returned through a buffer, plus one per
// result-area parameter.
func resultCount(function *ir.Func) int {
	count := 0
	for _, parameter := range function.Params {
		if parameter != nil && strings.HasPrefix(parameter.Name, "result") {
			count++
		}
	}
	if function.HasRet || function.RetAgg != nil {
		parts := 1
		for _, block := range function.Blocks {
			if block.Jmp.Kind == ir.JmpRet && 1+len(block.Jmp.Args) > parts {
				parts = 1 + len(block.Jmp.Args)
			}
		}
		if parts > count {
			count = parts
		}
	}
	return count
}

func (graph *escapeGraph) resultLoc(index int) int {
	if index < 0 || index >= graph.results {
		return escapeHeapLoc
	}
	return 1 + index
}

func (graph *escapeGraph) tempLoc(id uint32) int {
	if int(id) >= len(graph.function.Temps) {
		return -1
	}
	return graph.tempBase + int(id)
}

// regionLoc is the location of one frame allocation: the whole allocation, not
// one word of it.
//
// Offsets are deliberately collapsed. An allocation whose address is published
// is published entirely -- goc emits a closure environment as one alloc8 32 and
// stores the captured variables at 8, 16 and 24, so escaping only the word the
// published pointer happened to name would miss every capture. It is also what
// cmd/compile does: escape.addr resolves a field selection to the whole
// variable's location.
//
// The precision this gives up is between fields of one aggregate. It costs
// nothing between variables, because goc gives each variable its own OAlloc.
func (graph *escapeGraph) regionLoc(base locKey) int {
	if id, known := graph.regions[base]; known {
		return id
	}
	id := len(graph.edges)
	graph.regions[base] = id
	graph.edges = append(graph.edges, nil)
	return id
}

func (graph *escapeGraph) addEdge(dst, src int, derefs int32) {
	if dst < 0 || src < 0 || dst == src {
		return
	}
	graph.edges[dst] = append(graph.edges[dst], escapeEdge{src: int32(src), derefs: derefs})
}

// flow records that dereferencing src derefs times produces a value assigned to
// dst. Only temporaries carry values here; a constant symbol is a global
// address, which no parameter can be.
//
// A src that scalarValue rules out as a pointer carries nothing at derefs 0 and
// is not recorded.
//
// Dropping those edges matters because regions here are field-insensitive. A
// slice parameter is reconstructed into one frame slot, so the load of its
// length and the load of its data pointer come out of the same node, and
// without the test any arithmetic on the length -- `len(a) - 1`, which is what
// a reslice computes -- published the data pointer. That was enough to make
// every `[]any` parameter in fmt escape, and with it every variadic call to
// fmt.
func (graph *escapeGraph) flow(dst int, src ir.Ref, derefs int32) {
	if src.Kind != ir.RefTemp {
		return
	}
	if derefs == 0 && graph.scalarValue(src) {
		return
	}
	graph.addEdge(dst, graph.tempLoc(src.ID), derefs)
}

// scalarValue reports that a value cannot be a pointer, so publishing it
// publishes nothing.
//
// cg12 keeps a distinct abstract pointer class, ir.ClsP, right up to the
// backend's LowerPointers, so a value of any other class is nominally an
// integer, a length, a float. The class alone is not enough to conclude
// anything, though: the front end coerces a pointer into a width class when it
// feeds one to a width-typed intrinsic, and sync/atomic.StorePointer really does
// end `%p =p load ...; %l =l copy %p; intrinsic atomic.store.l`. Reading that
// copy as a scalar loses the store, leaves the pointed-at object in a frame,
// and hands the caller a dangling pointer -- which is exactly what it did, to
// log.Logger.SetPrefix.
//
// So the answer is no if any copy or cast the value was narrowed from is of
// pointer class. Everything else -- an arithmetic result, a load the front end
// typed as a scalar, a constant -- is a value this function computed rather
// than a pointer wearing a different class, and a parameter or phi, whose
// definition is not an instruction, is answered conservatively.
func (graph *escapeGraph) scalarValue(value ir.Ref) bool {
	for hops := 0; hops < 32; hops++ {
		if value.Kind != ir.RefTemp || graph.function.ClassOf(value) == ir.ClsP {
			return false
		}
		definition, defined := graph.definitions[value.ID]
		if !defined {
			return false
		}
		if definition.Op != ir.OCopy && definition.Op != ir.OCast {
			return true
		}
		value = definition.Arg(0)
	}
	return false
}

func (graph *escapeGraph) build() {
	graph.definitions = make(map[uint32]ir.Instr, len(graph.function.Temps))
	for _, block := range graph.function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.To.Kind == ir.RefTemp {
				graph.definitions[instruction.To.ID] = instruction
			}
		}
	}

	// A temporary that names storage in this frame holds that storage's
	// address, so it is one dereference *below* the slot it names. This is the
	// edge that connects the memory form frontend variables have -- an OAlloc
	// slot written and read back -- to the value form the rest of the graph is
	// in.
	for id := range graph.function.Temps {
		if graph.function.Temps[id] == nil {
			continue
		}
		location := graph.aliases.locOf(ir.Ref{Kind: ir.RefTemp, ID: uint32(id)}, 1)
		if location.keyKind != keyLocal && location.keyKind != keyEscaped {
			continue
		}
		graph.addEdge(graph.tempLoc(uint32(id)), graph.regionLoc(location.key()), -1)
	}

	for _, block := range graph.function.Blocks {
		for _, phi := range block.Phis {
			if phi.To.Kind != ir.RefTemp {
				continue
			}
			for _, argument := range phi.Args {
				graph.flow(graph.tempLoc(phi.To.ID), argument, 0)
			}
		}
		for _, instruction := range block.Instrs {
			graph.instruction(instruction)
		}
		graph.terminator(block)
	}
}

func (graph *escapeGraph) instruction(instruction ir.Instr) {
	result := -1
	if instruction.To.Kind == ir.RefTemp {
		result = graph.tempLoc(instruction.To.ID)
	}
	switch {
	case instruction.Op == ir.OHeapAlloc || instruction.Op.IsAlloc() || instruction.Op == ir.OAllocN:
		// Fresh storage. The operands are an allocator, a type descriptor and a
		// size, none of which is a use of a pointer this function was handed.
	case instruction.Op.IsLoad():
		graph.flow(result, instruction.Arg(0), 1)
	case instruction.Op.IsStore():
		graph.store(instruction.Arg(1), instruction.Arg(0))
	case instruction.Op == ir.OCmp:
		// Comparing a pointer observes its value but cannot retain it.
	case instruction.Op == ir.OCopy || instruction.Op == ir.OCast:
		graph.flow(result, instruction.Arg(0), 0)
	case (instruction.Op == ir.OAdd || instruction.Op == ir.OSub) && instruction.Cls == ir.ClsP:
		graph.flow(result, instruction.Arg(0), 0)
		if instruction.Op == ir.OAdd {
			graph.flow(result, instruction.Arg(1), 0)
		}
	case instruction.Op == ir.OSel && len(instruction.Args) >= 3:
		graph.flow(result, instruction.Arg(1), 0)
		graph.flow(result, instruction.Arg(2), 0)
	case instruction.Op == ir.OBlit && len(instruction.Args) >= 2:
		graph.copyMemory(instruction.Arg(1))
	case isAtomicPointerStore(graph.function, instruction):
		// The write barrier stores Args[2] through Args[1] and retains nothing
		// else.
		graph.store(instruction.Arg(1), instruction.Arg(2))
	case instruction.Op == ir.OCall:
		graph.call(instruction)
	default:
		for _, argument := range instruction.Args {
			graph.flow(escapeHeapLoc, argument, 0)
		}
	}
}

// store records an assignment through a pointer. A destination this function's
// frame owns is a location of its own; a destination reached through a result
// parameter is the caller's result area, which is a leak to that result;
// anything else outlives the call and is a publication.
func (graph *escapeGraph) store(address, value ir.Ref) {
	location := graph.aliases.locOf(address, 1)
	switch location.keyKind {
	case keyLocal, keyEscaped:
		region := graph.regionLoc(location.key())
		if graph.closureEnvironment(value) {
			graph.forced = append(graph.forced, region)
		}
		graph.flow(region, value, 0)
		return
	case keyTemp:
		if index, isResult := graph.resultParams[uint32(location.keyID)]; isResult {
			graph.flow(graph.resultLoc(index), value, 0)
			return
		}
	}
	graph.flow(escapeHeapLoc, value, 0)
}

// copyMemory records a block copy out of source. The destination is not
// modelled: whatever the copy lands in, the contents of source -- one
// dereference below it -- have been published, and that is the level at which
// the fact is true whether the destination turns out to be a frame slot or a
// heap object. Modelling it as a publication of the source's pointee rather
// than of the source keeps `*dst = *src` from claiming that src itself escapes.
func (graph *escapeGraph) copyMemory(source ir.Ref) {
	graph.flow(escapeHeapLoc, source, 1)
}

func (graph *escapeGraph) call(instruction ir.Instr) {
	callee := constSymbolName(graph.function, instruction.Arg(0))
	if benignMemoryCall(graph.function, instruction) {
		switch callee {
		case "memset", "goc_memset", "memcmp", "goc_memcmp":
			// One writes a constant and the other only reads. Neither retains an
			// argument and neither copies one pointer-bearing region into
			// another.
		case "runtime.growslice":
			// Reallocates storage it was handed, copying the old backing array
			// into fresh heap storage.
			for _, argument := range instruction.Args[1:] {
				graph.copyMemory(argument)
			}
		default:
			if _, source, _, isCopy := memoryCopyOperands(graph.function, instruction); isCopy {
				graph.copyMemory(source)
			} else {
				// A copy whose size is not a constant: the same publication, with
				// no bound on how much of the source is read.
				graph.copyMemory(instruction.Arg(2))
			}
		}
		return
	}

	if calleeRetainsNothing(graph.function, callee) {
		return
	}
	target, arguments, usable := summarisedCallee(graph.byName, callee, instruction)
	for index, argument := range arguments {
		if !usable {
			graph.flow(escapeHeapLoc, argument, 0)
			continue
		}
		// Whatever the summary says, it is a claim about the pointer value and
		// about nothing else: summary() reads a parameter as ParamNoEscape
		// exactly when the heap reaches it at a dereference count of one or
		// more, which is to say "the callee keeps something reachable through
		// this, but not this". Consuming that as "the callee keeps nothing"
		// loses the difference, and the difference is the whole point of
		// carrying a depth (see this file's doc comment).
		//
		// It goes wrong wherever the argument is the address of storage this
		// frame owns -- goc's calling convention for any aggregate wider than a
		// couple of words, and every &x -- because then "reachable through the
		// argument" is the frame slot's contents. Publishing the pointee at
		// depth 1 states the summary's actual claim: I believe you do not keep
		// the pointer, I do not believe you keep nothing it points at.
		//
		// It costs no precision on an ordinary pointer argument. A parameter
		// forwarded to a noescape callee is still reached at depth 1 and still
		// summarises as ParamNoEscape; a pointer loaded out of a frame slot is
		// reached at depth 2. Only an address handed over directly is affected,
		// which is exactly the case that was wrong.
		graph.flow(escapeHeapLoc, argument, 1)
		switch fact := graph.facts.Param(callee, index); fact.Escape {
		case ParamNoEscape:
			// Nothing further: the callee cannot retain the pointer itself.
		case ParamLeaksToResult:
			if callLeaksToTrackedResult(instruction, target, fact) {
				graph.flow(graph.tempLoc(instruction.To.ID), argument, 0)
				continue
			}
			graph.flow(escapeHeapLoc, argument, 0)
		default:
			graph.flow(escapeHeapLoc, argument, 0)
		}
	}
}

// closureEnvironment reports that a value being stored is a function symbol,
// which is how goc builds a closure: word 0 of the environment is the code
// address and the captured variables follow it.
//
// Everything in such an environment has to be treated as escaping. The captures
// are read back through the closure body's %closure register, in a different
// ir.Func, and this analysis has no edge from an environment to the body that
// reads it -- so it cannot see that runtime.newproc's fn, stored into the
// environment of the literal it hands to systemstack, is retained by the
// goroutine that literal starts. gc gets this right by analysing a func literal
// in the same batch as its enclosing function; on the IR the literal is a
// separate function and the link is the register, which no dataflow over one
// function can follow.
//
// systemstack is //go:noescape and correctly so -- it runs fn and returns -- so
// nothing else in the chain would have caught it. Without this rule the summary
// says runtime.newproc retains nothing, which is wrong in the permissive
// direction.
func (graph *escapeGraph) closureEnvironment(value ir.Ref) bool {
	if value.Kind != ir.RefConst || graph.byName == nil {
		return false
	}
	symbol := constSymbolName(graph.function, value)
	if symbol == "" {
		return false
	}
	return graph.byName[symbol] != nil
}

func (graph *escapeGraph) terminator(block *ir.Block) {
	if block.Jmp.Kind != ir.JmpRet {
		// A branch observes a condition; it publishes nothing.
		return
	}
	if graph.function.RetAgg != nil && !graph.function.RetValues {
		// Jmp.Arg is the address of the aggregate; its contents are copied into
		// the caller's storage on the way out.
		graph.flow(graph.resultLoc(0), block.Jmp.Arg, 1)
		return
	}
	graph.flow(graph.resultLoc(0), block.Jmp.Arg, 0)
	for index, argument := range block.Jmp.Args {
		graph.flow(graph.resultLoc(index+1), argument, 0)
	}
}

// solve walks the graph from every root that outlives the frame. The heap node
// is such a root; so is each result; and so is any location whose address turns
// out to reach one, which is why a root that escapes mid-solve is pushed back
// onto the worklist.
func (graph *escapeGraph) solve() {
	graph.escapes = make([]bool, len(graph.edges))
	graph.escapes[escapeHeapLoc] = true

	roots := make([]int, 0, 1+graph.results+len(graph.forced))
	for index := graph.results - 1; index >= 0; index-- {
		roots = append(roots, graph.resultLoc(index))
	}
	for _, location := range graph.forced {
		if !graph.escapes[location] {
			graph.escapes[location] = true
			roots = append(roots, location)
		}
	}
	roots = append(roots, escapeHeapLoc)

	derefs := make([]int32, len(graph.edges))
	seen := make([]uint32, len(graph.edges))
	var generation uint32
	for len(roots) > 0 {
		root := roots[len(roots)-1]
		roots = roots[:len(roots)-1]
		generation++
		roots = graph.walkOne(root, generation, derefs, seen, roots)
	}
}

// walkOne is Bellman-Ford from one root. Dereference counts are lower bounded
// at zero when a location is popped, which both matches gc's rule -- for
// "root = &l; l = x", l's address flows to root but x's does not -- and keeps
// the negative edges from cycling.
func (graph *escapeGraph) walkOne(root int, generation uint32, derefs []int32, seen []uint32, roots []int) []int {
	rootEscapes := graph.escapes[root]
	seen[root] = generation
	derefs[root] = 0
	todo := []int{root}
	for len(todo) > 0 {
		location := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if seen[location] != generation {
			continue
		}
		distance := derefs[location]
		addressOf := distance < 0
		if addressOf {
			distance = 0
		}
		if index, isParam := graph.paramIndex[location]; isParam {
			graph.recordLeak(index, root, rootEscapes, distance)
		}
		if addressOf && !graph.escapes[location] {
			// The location's address reaches something that outlives it, so the
			// storage itself has to. Everything stored into it now escapes, which
			// is what re-rooting the walk here computes.
			graph.escapes[location] = true
			roots = append(roots, location)
			continue
		}
		for _, edge := range graph.edges[location] {
			if graph.escapes[edge.src] {
				continue
			}
			next := distance + edge.derefs
			if seen[edge.src] != generation || derefs[edge.src] > next {
				seen[edge.src] = generation
				derefs[edge.src] = next
				todo = append(todo, int(edge.src))
			}
		}
	}
	return roots
}

func (graph *escapeGraph) recordLeak(parameter, root int, rootEscapes bool, distance int32) {
	if root == escapeHeapLoc || rootEscapes {
		if distance < graph.heapLeak[parameter] {
			graph.heapLeak[parameter] = distance
		}
		return
	}
	index := root - 1
	if index < 0 || index >= graph.results {
		if distance < graph.heapLeak[parameter] {
			graph.heapLeak[parameter] = distance
		}
		return
	}
	if graph.resultLeak[parameter] == nil {
		graph.resultLeak[parameter] = make(map[int]int32, 1)
	}
	if previous, known := graph.resultLeak[parameter][index]; !known || distance < previous {
		graph.resultLeak[parameter][index] = distance
	}
}

// summary reads the walk out as one fact per parameter. Only a leak at
// dereference count zero is about the pointer itself: at one or more it is what
// the pointer points at that outlives the call, and the caller's own object can
// still stay in a frame.
func (graph *escapeGraph) summary() []ParamFact {
	facts := make([]ParamFact, len(graph.function.Params))
	for index := range facts {
		if graph.heapLeak[index] <= 0 {
			facts[index] = ParamFact{Escape: ParamEscapes}
			continue
		}
		leaked := -1
		multiple := false
		for result, distance := range graph.resultLeak[index] {
			if distance > 0 {
				continue
			}
			if leaked >= 0 && leaked != result {
				multiple = true
			}
			if leaked < 0 || result < leaked {
				leaked = result
			}
		}
		// Deep is the same question one dereference further out: is anything
		// reachable through the parameter retained, rather than the pointer
		// itself. heapLeak is the smallest depth at which the heap reaches the
		// parameter, so "greater than one" is "not even the pointee", and the
		// result leaks have to clear the same bar.
		deep := graph.heapLeak[index] > 1
		for _, distance := range graph.resultLeak[index] {
			if distance <= 1 {
				deep = false
				break
			}
		}
		switch {
		case multiple:
			facts[index] = ParamFact{Escape: ParamEscapes}
		case leaked >= 0:
			facts[index] = ParamFact{Escape: ParamLeaksToResult, Result: leaked}
		default:
			facts[index] = ParamFact{Escape: ParamNoEscape, Deep: deep}
		}
	}
	return facts
}
