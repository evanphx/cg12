package opt

import "github.com/evanphx/cg12/ir"

// callGraph is a module's direct-call graph: functions are nodes, and a direct
// call — an OCall whose callee is a symbol naming a module function — is an
// edge. Indirect and external calls resolve to no known callee and are not
// edges.
type callGraph struct {
	funcs   []*ir.Func
	byName  map[string]*ir.Func
	callees map[*ir.Func][]*ir.Func // deduplicated direct callees
	selfRec map[*ir.Func]bool       // has a direct call to itself
}

// buildCallGraph derives the direct-call graph of m.
func buildCallGraph(m *ir.Module) *callGraph {
	cg := &callGraph{
		funcs:   m.Funcs,
		byName:  make(map[string]*ir.Func, len(m.Funcs)),
		callees: make(map[*ir.Func][]*ir.Func, len(m.Funcs)),
		selfRec: make(map[*ir.Func]bool),
	}
	for _, f := range m.Funcs {
		cg.byName[f.Name] = f
	}
	for _, f := range m.Funcs {
		seen := make(map[*ir.Func]bool)
		for _, b := range f.Blocks {
			for i := range b.Instrs {
				in := &b.Instrs[i]
				if in.Op != ir.OCall {
					continue
				}
				g := directCallee(f, in, cg.byName)
				if g == nil {
					continue
				}
				if g == f {
					cg.selfRec[f] = true
				}
				if !seen[g] {
					seen[g] = true
					cg.callees[f] = append(cg.callees[f], g)
				}
			}
		}
	}
	return cg
}

// sccInfo is the strongly-connected-component structure of a call graph. A
// non-trivial SCC (more than one function) is mutual recursion; a single
// function that calls itself is direct recursion. Both are flagged recursive.
type sccInfo struct {
	comp      map[*ir.Func]int  // function -> component id
	recursive map[*ir.Func]bool // participates in a recursion cycle
	// order lists functions bottom-up (reverse topological order of the SCC
	// condensation): every function a function calls — outside its own SCC —
	// appears earlier, so a callee is finalized before its callers inline it.
	order []*ir.Func
}

// computeSCC runs Tarjan's algorithm over the call graph. Tarjan emits SCCs in
// reverse topological order, which is exactly the bottom-up order the inliner
// wants, so `order` is produced as a side effect.
func computeSCC(cg *callGraph) *sccInfo {
	s := &sccInfo{
		comp:      make(map[*ir.Func]int, len(cg.funcs)),
		recursive: make(map[*ir.Func]bool, len(cg.funcs)),
	}
	var (
		counter int
		nextID  int
		index   = make(map[*ir.Func]int, len(cg.funcs))
		lowlink = make(map[*ir.Func]int, len(cg.funcs))
		onStack = make(map[*ir.Func]bool, len(cg.funcs))
		stack   []*ir.Func
	)
	var connect func(v *ir.Func)
	connect = func(v *ir.Func) {
		index[v] = counter
		lowlink[v] = counter
		counter++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range cg.callees[v] {
			if _, ok := index[w]; !ok {
				connect(w)
				if lowlink[w] < lowlink[v] {
					lowlink[v] = lowlink[w]
				}
			} else if onStack[w] {
				if index[w] < lowlink[v] {
					lowlink[v] = index[w]
				}
			}
		}
		if lowlink[v] != index[v] {
			return // not an SCC root yet
		}
		id := nextID
		nextID++
		var members []*ir.Func
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			s.comp[w] = id
			s.order = append(s.order, w)
			members = append(members, w)
			if w == v {
				break
			}
		}
		mutual := len(members) > 1
		for _, w := range members {
			if mutual || cg.selfRec[w] {
				s.recursive[w] = true
			}
		}
	}
	for _, f := range cg.funcs {
		if _, ok := index[f]; !ok {
			connect(f)
		}
	}
	return s
}
