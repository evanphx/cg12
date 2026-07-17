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
	return nil
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
