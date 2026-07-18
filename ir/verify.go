package ir

import "fmt"

// Verify checks a function's structural invariants: that every reference names
// something, every op that needs a payload has one, and every block ends.
//
// The IR has three front doors -- the Go builder, the text parser, and the
// binary decoder -- and they do not agree about what the IR is. The builder can
// only construct a well-formed OAsm, because Block.Asm takes the template as an
// argument. The parser will build one from the word "asm" alone, with a nil
// payload, because it accepts any name in the op table and checks nothing else;
// the backend then dereferences that nil and panics. The decoder used to drop
// the payload of a perfectly good one.
//
// So the invariants an emitter relies on are real, and nothing enforced them.
// This is the enforcement: a front door calls it, and a malformed function is a
// diagnostic naming the instruction rather than a panic somewhere in a backend.
//
// Verify does not check the things lowering establishes -- ABI shapes, register
// assignments, whether phis have been destructed. It checks what is true of
// every function at every stage.
func Verify(f *Func) error {
	for _, b := range f.Blocks {
		if b.Jmp.Kind == JmpNone {
			return fmt.Errorf("ir: %s: block %q has no terminator", f.Name, b.Name)
		}
		for _, p := range b.Phis {
			if len(p.Args) != len(p.Blocks) {
				return fmt.Errorf("ir: %s: block %q: phi has %d arguments for %d predecessors",
					f.Name, b.Name, len(p.Args), len(p.Blocks))
			}
			for _, a := range p.Args {
				if err := verifyRef(f, b, "phi", a); err != nil {
					return err
				}
			}
		}
		for i := range b.Instrs {
			if err := verifyInstr(f, b, &b.Instrs[i]); err != nil {
				return err
			}
		}
		if err := verifyRef(f, b, "terminator", b.Jmp.Arg); err != nil {
			return err
		}
	}
	// The SSA shape -- one definition per temp, every use defined, phis that match
	// the control flow -- holds only before lowering destructs it into machine
	// registers. After that a temp is reassigned freely and phis are gone, so these
	// checks would reject the very form lowering produces.
	if f.LoweredFor() == "" {
		if err := verifySSA(f); err != nil {
			return err
		}
	}
	return nil
}

// verifySSA checks the well-formedness an optimizer assumes but the reference
// bounds check in verifyRef does not establish: that every temporary is assigned
// exactly once, that no use reads a temporary nothing defines, and that phis and
// the entry agree with the control-flow graph. A temp that is in range but never
// defined, or a phi naming a block that does not branch to it, passes structural
// verification yet sends a dominance-frontier pass (mem2reg) into a loop.
func verifySSA(f *Func) error {
	// One definition per temporary; collect them all first.
	defined := make([]bool, len(f.Temps))
	def := func(what string, r Ref) error {
		if r.Kind != RefTemp {
			return nil
		}
		if int(r.ID) >= len(defined) {
			return nil // verifyRef already reported the out-of-range id
		}
		if defined[r.ID] {
			return fmt.Errorf("ir: %s: %%%d is assigned more than once (not in SSA form)", f.Name, r.ID)
		}
		defined[r.ID] = true
		return nil
	}
	for _, p := range f.Params {
		if p.ID < len(defined) {
			defined[p.ID] = true
		}
	}
	for _, b := range f.Blocks {
		for _, p := range b.Phis {
			if err := def("phi", p.To); err != nil {
				return err
			}
		}
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if err := def(in.Op.String(), in.To); err != nil {
				return err
			}
			for _, d := range in.Defs {
				if err := def(in.Op.String(), d); err != nil {
					return err
				}
			}
		}
	}
	// Every temporary a use reads must have a definition somewhere.
	use := func(where, what string, r Ref) error {
		if r.Kind == RefTemp && int(r.ID) < len(defined) && !defined[r.ID] {
			return fmt.Errorf("ir: %s: %s: %s reads %%%d, which nothing defines", f.Name, where, what, r.ID)
		}
		return nil
	}
	for _, b := range f.Blocks {
		for _, p := range b.Phis {
			for _, a := range p.Args {
				if err := use(b.Name, "phi", a); err != nil {
					return err
				}
			}
		}
		for i := range b.Instrs {
			in := &b.Instrs[i]
			for _, a := range in.Args {
				if err := use(b.Name, in.Op.String(), a); err != nil {
					return err
				}
			}
		}
		if err := use(b.Name, "terminator", b.Jmp.Arg); err != nil {
			return err
		}
		for _, a := range b.Jmp.Args {
			if err := use(b.Name, "terminator", a); err != nil {
				return err
			}
		}
	}
	return verifyPhiEdges(f)
}

// verifyPhiEdges checks that every phi's incoming blocks are exactly the block's
// control-flow predecessors -- the premise a phi encodes, which a fuzzed or
// hand-built module can violate while still passing the argument-count check. (The
// entry may itself be a loop header, so it is allowed predecessors.)
func verifyPhiEdges(f *Func) error {
	preds := make(map[*Block]map[*Block]bool, len(f.Blocks))
	for _, b := range f.Blocks {
		preds[b] = map[*Block]bool{}
	}
	for _, b := range f.Blocks {
		for _, s := range b.Succs() {
			if s != nil && preds[s] != nil {
				preds[s][b] = true
			}
		}
	}
	for _, b := range f.Blocks {
		for _, p := range b.Phis {
			seen := map[*Block]bool{}
			for _, from := range p.Blocks {
				if !preds[b][from] {
					return fmt.Errorf("ir: %s: block %q: phi names %q as a predecessor, but it does not branch here",
						f.Name, b.Name, blockLabel(from))
				}
				seen[from] = true
			}
			for from := range preds[b] {
				if !seen[from] {
					return fmt.Errorf("ir: %s: block %q: phi has no argument for predecessor %q",
						f.Name, b.Name, from.Name)
				}
			}
		}
	}
	return nil
}

func blockLabel(b *Block) string {
	if b == nil {
		return "<nil>"
	}
	return b.Name
}

func verifyInstr(f *Func, b *Block, in *Instr) error {
	where := func(what string, a ...any) error {
		return fmt.Errorf("ir: %s: block %q: %s: %s", f.Name, b.Name, in.Op, fmt.Sprintf(what, a...))
	}
	// An op whose meaning lives outside Args must actually carry it. These are
	// the two an emitter dereferences without asking.
	switch in.Op {
	case OAsm:
		if in.Asm == nil {
			return where("no template: an inline asm is its template, and there is no default")
		}
		if len(in.Asm.Regs) != 0 && len(in.Asm.Regs) != len(in.Asm.Ops) {
			return where("%d operands but %d register constraints", len(in.Asm.Ops), len(in.Asm.Regs))
		}
	case OBlockAddr:
		if in.Blk == nil {
			return where("no target block: the address of a label needs the label")
		}
	}
	for _, a := range in.Args {
		if err := verifyRef(f, b, in.Op.String(), a); err != nil {
			return err
		}
	}
	if err := verifyRef(f, b, in.Op.String(), in.To); err != nil {
		return err
	}
	for _, d := range in.Defs {
		if err := verifyRef(f, b, in.Op.String(), d); err != nil {
			return err
		}
	}
	return nil
}

// verifyRef checks that a reference names something that exists. A temp id past
// the end of Temps indexes out of range in whichever pass reaches it first.
func verifyRef(f *Func, b *Block, what string, r Ref) error {
	switch r.Kind {
	case RefTemp:
		if int(r.ID) >= len(f.Temps) {
			return fmt.Errorf("ir: %s: block %q: %s: temporary %%%d does not exist (%d in this function)",
				f.Name, b.Name, what, r.ID, len(f.Temps))
		}
	case RefConst:
		if int(r.ID) >= len(f.Consts) {
			return fmt.Errorf("ir: %s: block %q: %s: constant %d does not exist (%d in this function)",
				f.Name, b.Name, what, r.ID, len(f.Consts))
		}
	}
	return nil
}

// VerifyModule verifies every function in a module.
func VerifyModule(m *Module) error {
	for _, f := range m.Funcs {
		if err := Verify(f); err != nil {
			return err
		}
	}
	return nil
}
