package cc

import "modernc.org/cc/v4"

// cg12 does not implement atomics, and this is where it says so.
//
// It used to compile them, which was worse. `_Atomic int g; g++;` became a plain
// loaduw/add/storew -- a read-modify-write with no atomicity at all -- and
// __atomic_load_n(&g, __ATOMIC_SEQ_CST) became a plain load, its memory-order
// argument evaluated and dropped. Both produced a program that runs, passes a
// single-threaded test, and races under contention: the failure appears on
// someone else's machine, months later, as corruption with no explanation.
//
// The IR has nothing to build atomics out of yet. There is no fence: the only
// barrier cg12 models is the compiler barrier that inline asm's "memory" clobber
// provides (see cc/asm.go), which stops cg12 reordering and emits no instruction,
// so it does not stop the processor. There are no atomic RMW or compare-exchange
// ops either. Until the IR grows an ordering and the ops to carry it, an error is
// the only honest answer.
//
// The __atomic_fetch_* and __atomic_compare_exchange* families already failed,
// but at link time and dishonestly: they became calls to symbols nobody defines,
// so the report was `undefined symbol "__atomic_fetch_add"` -- which reads as a
// missing library rather than as a compiler that does not implement the thing.

// atomicUnsupported is the shared explanation. Every one of these paths is a
// program that would compile to code that looks right and is not.
func (g *gen) atomicUnsupported(what string) {
	g.fail("cc: %s is not supported: cg12 has no atomic operations and no fence "+
		"in its IR, so this would compile to ordinary loads and stores -- a "+
		"program that races rather than one that fails", what)
}

// checkAtomicDecl refuses an _Atomic object. C says every access to it is atomic;
// cg12 would give it ordinary ones.
func (g *gen) checkAtomicDecl(d *cc.Declarator) {
	if d != nil && d.IsAtomic() {
		g.atomicUnsupported("_Atomic " + d.Name())
	}
}

// checkAtomicMember refuses an access to an _Atomic struct or union member: the
// same object with the same promise, reached through a field rather than a name.
//
// It asks about the member being touched rather than about the whole aggregate.
// A struct may hold an atomic member beside ordinary ones, and reading an
// ordinary one is an ordinary load -- refusing that would refuse programs cg12
// compiles correctly.
func (g *gen) checkAtomicMember(f *cc.Field) {
	if f == nil {
		return
	}
	if d := f.Declarator(); d != nil && d.IsAtomic() {
		g.atomicUnsupported("the _Atomic member " + f.Name())
	}
}
