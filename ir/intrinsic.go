package ir

// An intrinsic is a named low-level primitive invoked by an OIntrinsic. Rather
// than give each such primitive its own opcode -- threaded by hand through the
// verifier, both interpreters, the optimizer, and every backend -- an intrinsic
// is a name plus a description of how it behaves. The name dispatches execution
// and lowering (a switch in the interpreter and in each backend that supports
// it); the description here is what the optimizer reads to reason about a call it
// does not otherwise understand.
//
// The description is deliberately in terms of what the passes actually consult:
// may an unused one be deleted (DCE), may two equal ones be shared (GVN), may one
// be scheduled elsewhere (GCM), does it read or clobber tracked memory (load
// elimination and alias analysis). An intrinsic that leaves every flag at its
// zero value is described as doing nothing observable -- so anything that touches
// state must say so, or the optimizer will wrongly treat it as dead and movable.

// IntrinsicEffects describes an intrinsic's observable behavior for the optimizer.
type IntrinsicEffects struct {
	// HasResult reports that the intrinsic defines a value (carried in Instr.To).
	HasResult bool

	// SideEffect marks an intrinsic whose execution matters even when its result
	// is unused: dead-code elimination must keep it, and it is never moved. Set it
	// for anything that changes state a later instruction can observe -- the stack
	// pointer, a device register, control flow. It is distinct from Pure: a value
	// can be impure (not shareable) yet have no side effect (an unused one is dead).
	SideEffect bool

	// Pure marks an intrinsic whose result is a function of its operands alone,
	// with no dependence on memory or any mutable state. A pure intrinsic can be
	// value-numbered -- two calls with equal operands collapse to one -- and freely
	// rescheduled. It is mutually exclusive with SideEffect and the memory flags.
	// "stacksave" is NOT pure: it reads the live stack pointer, whose value depends
	// on where in the program the read happens.
	Pure bool

	// ReadsMemory / WritesMemory report that the intrinsic reads, or writes, memory
	// the optimizer tracks. A writer clobbers cached loads the way a call does. A
	// reader must not be treated as pure. (Reads currently only forbid purity;
	// forwarding a value through an intrinsic reader is not modelled.)
	ReadsMemory  bool
	WritesMemory bool

	// EscapesArgs reports that a pointer operand may become reachable elsewhere --
	// stored, returned, or handed to something outside the function. When false,
	// alias analysis may keep treating the pointed-to storage as local across the
	// call; when true, the operands are conservatively assumed to escape.
	EscapesArgs bool
}

// Intrinsic is a registered intrinsic: its dispatch name and its effects.
type Intrinsic struct {
	Name string
	IntrinsicEffects
}

// IntrinOp is the payload an OIntrinsic carries (Instr.Intrin): the name of the
// intrinsic being invoked, which dispatches its execution and lowering and keys
// its registered effects.
type IntrinOp struct {
	Name string
}

var intrinsics = map[string]*Intrinsic{}

// RegisterIntrinsic records an intrinsic and its effects, returning the entry. It
// panics on a duplicate name so two registrations cannot silently disagree about
// how the same intrinsic behaves.
func RegisterIntrinsic(name string, eff IntrinsicEffects) *Intrinsic {
	if _, ok := intrinsics[name]; ok {
		panic("ir: intrinsic already registered: " + name)
	}
	in := &Intrinsic{Name: name, IntrinsicEffects: eff}
	intrinsics[name] = in
	return in
}

// LookupIntrinsic returns the registered intrinsic of the given name, or nil when
// no intrinsic by that name is registered.
func LookupIntrinsic(name string) *Intrinsic { return intrinsics[name] }

func init() {
	// stacksave reads the current stack pointer. It has a result but no side
	// effect -- an unused one is dead -- and it is impure: two reads at different
	// points see different stack pointers, so they must not be shared or moved.
	RegisterIntrinsic("stacksave", IntrinsicEffects{HasResult: true})

	// stackrestore sets the stack pointer from a saved value, reclaiming any VLAs
	// allocated since. That is a side effect: it must be kept and never moved.
	RegisterIntrinsic("stackrestore", IntrinsicEffects{SideEffect: true})

	registerAtomics()
}

// registerAtomics registers the atomic memory operations. Each is a
// synchronization primitive, so every one is a SideEffect: never removed even if
// its result is unused, never moved, never shared -- reordering or eliminating an
// atomic breaks the memory model. The operation and its access width are encoded
// in the name (e.g. "atomic.add.l"), so the width needs no separate field and a
// void store still round-trips through the textual IL.
func registerAtomics() {
	// A read-modify-write touches memory both ways and, because it can publish a
	// pointer operand into shared memory, escapes its arguments.
	rmw := IntrinsicEffects{
		HasResult: true, SideEffect: true,
		ReadsMemory: true, WritesMemory: true, EscapesArgs: true,
	}
	for _, w := range []string{"w", "l"} {
		// A pure load reads memory; a pure store writes it (and may publish a
		// pointer value, so its operands escape).
		RegisterIntrinsic("atomic.load."+w, IntrinsicEffects{
			HasResult: true, SideEffect: true, ReadsMemory: true,
		})
		RegisterIntrinsic("atomic.store."+w, IntrinsicEffects{
			SideEffect: true, WritesMemory: true, EscapesArgs: true,
		})
		RegisterIntrinsic("atomic.xchg."+w, rmw)
		RegisterIntrinsic("atomic.cas."+w, rmw)
		for _, op := range []string{"add", "sub", "and", "or", "xor"} {
			RegisterIntrinsic("atomic."+op+"."+w, rmw)
		}
	}
	// A fence orders memory without touching a specific location.
	RegisterIntrinsic("atomic.fence", IntrinsicEffects{SideEffect: true})
}
