package amd64

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
)

// This file holds the System V AMD64 calling convention itself: which register or
// stack slot each argument lands in, which register a result comes back in, and
// the pinned temporaries that make those choices explicit in the IR. It is
// deliberately separate from lower.go, which decides *what* to move across a call
// boundary; the rules here decide *where* those values live. Keeping the two
// apart means a second convention (Go's ABIInternal, with its own register
// sequences and stack packing) is a new table here rather than another branch
// threaded through the lowering.

// roundUp rounds n up to the next multiple of a.
func roundUp(n, a int) int {
	if a <= 0 {
		return n
	}
	return ((n + a - 1) / a) * a
}

// abiConvention captures the register and stack-layout rules that vary with a
// function's calling convention -- the ABI axis. It is deliberately orthogonal to
// the frame axis (managed vs fixed stack, carried by ir.Func.UsesManagedFrame),
// which governs the morestack prologue, the outgoing-argument area, and GC
// marking.
//
// It holds register *tables* where arm64's equivalent holds counts. arm64 can
// count because its argument registers are X0..Xn-1 and V0..Vn-1, contiguous
// under both conventions; amd64's are neither contiguous nor in encoding order
// under either one (System V starts at RDI, ABIInternal at RAX and skips RDX),
// so the sequence has to be written out.
type abiConvention struct {
	// intArgRegs and floatArgRegs are the argument registers, in assignment
	// order. Integer and float arguments consume the two banks independently.
	intArgRegs   []Reg
	floatArgRegs []Reg
	// retIntRegs and retFloatRegs are the *result* registers, in assignment
	// order. They are a separate table rather than a reuse of the argument one
	// because the two conventions disagree about whether results and arguments
	// share a sequence: System V returns in RAX/RDX and XMM0/XMM1, which are not
	// its first argument registers, while Go's ABIInternal returns in the
	// argument registers themselves with the banks restarted from zero
	// (RAX, RBX, RCX, ... / X0, X1, ...).
	retIntRegs   []Reg
	retFloatRegs []Reg
	// packsStackArgs tightly packs stacked arguments by their natural size (Go
	// ABIInternal); otherwise each scalar occupies one 8-byte slot (System V).
	packsStackArgs bool
	// savesCalleeRegs means a callee preserves the System V callee-saved set
	// (RBX, R12..R15). Go ABIInternal preserves nothing.
	savesCalleeRegs bool
	// stackLinkBytes is the frame-chain link reserved ahead of the stacked
	// arguments. It is zero under both conventions on amd64: the call
	// instruction itself pushes the return address, so the link the arm64 Go ABI
	// has to reserve by hand (goStackLinkSize) already exists here. Kept as a
	// field rather than dropped so the arm64 and amd64 descriptors stay
	// legible against each other.
	stackLinkBytes int
}

// abiConventions is indexed by ir.CallConvention.
var abiConventions = [...]abiConvention{
	ir.CallConvPlatform: {
		intArgRegs:      argGP,
		floatArgRegs:    argFP,
		retIntRegs:      retIntRegs,
		retFloatRegs:    retSSERegs,
		packsStackArgs:  false,
		savesCalleeRegs: true,
		stackLinkBytes:  0,
	},
	ir.CallConvGoInternal: {
		intArgRegs:      goArgGP,
		floatArgRegs:    goArgFP,
		retIntRegs:      goArgGP,
		retFloatRegs:    goArgFP,
		packsStackArgs:  true,
		savesCalleeRegs: false,
		stackLinkBytes:  0,
	},
}

// conventionABI returns the descriptor for a calling convention.
func conventionABI(cc ir.CallConvention) abiConvention { return abiConventions[cc] }

// emissionConvention returns the calling convention one function's *body* is
// emitted against, and is the single place that decision is made. Four consumers
// have to agree per function or an ABIInternal function is miscompiled, and each
// reads its table through this:
//
//   - gcalloc.go: the allocation order and the callee-saved preference
//     (intAllocOrderFor / floatAllocOrderFor / calleeSavedFor).
//   - frame.go: which used registers the prologue must save (calleeSavedFor).
//   - callersave.go: which registers a call destroys (callerClobberedForConv).
//   - mc.go: the emitter's scratch pair (scratchRegsFor), because ABIInternal
//     passes arguments in R10/R11 -- the System V scratch registers.
//
// It now returns the function's own convention: B1 landed the matching lowering,
// so lowerParams and lowerReturns build their assigners from this same switch and
// a CallConvGoInternal function really does receive its parameters in
// RAX/RBX/RCX/RDI/... and return in the argument registers. Before that lowering
// existed this returned the platform ABI unconditionally, because an ABIInternal
// register file underneath System V argument assignment would have miscompiled
// every closure goc emits -- the allocator handing out R10/R11 while parameters
// lived there, and the scratch pair moving to R12/R13 with no prologue saving
// them. The flip and the lowering are one change for that reason and must never
// be separated again.
//
// Note what this switch is NOT. It answers "what does *this body* look like from
// the inside", which is a property of one function. It is not the answer for a
// call *out* of that function: a call's convention is a property of its callee
// and is resolved by calleeConventions.forCall. A platform-ABI function calling
// an ABIInternal closure -- the ordinary shape in goc output -- must lower that
// call ABIInternal while its own body stays System V, and lowerCalls keeps the
// two apart for exactly that reason.
func emissionConvention(f *ir.Func) ir.CallConvention { return f.CallConv }

// goInternalConvention maps the backend's resolved "is this call Go-internal"
// boolean back to the convention it denotes, so sites that carry the per-call
// boolean can still look the descriptor up.
func goInternalConvention(goInternal bool) ir.CallConvention {
	if goInternal {
		return ir.CallConvGoInternal
	}
	return ir.CallConvPlatform
}

// calleeConventions resolves the convention each call site must be lowered
// against. It is built once per module because a direct call names its callee by
// symbol, and the callee's convention is a property of the other function.
//
// This deliberately does not reproduce arm64's rule. arm64 resolves an unmarked
// call by inheriting the *enclosing* function's convention
// (callUsesGoInternal), which is unsound: goc marks method-value wrappers,
// function literals, and funcvalue adapters CallConvGoInternal, and those
// functions make ordinary unmarked direct calls to plain platform-ABI functions.
// Inheriting there lowers the call as ABIInternal against a System V callee.
//
// arm64 survives it because both its conventions assign integer arguments from
// X0 upward, so the two lowerings agree for small argument counts and the bug
// stays latent. amd64 has no overlap at all -- System V starts at RDI,
// ABIInternal at RAX -- so the same rule would produce wrong code at the first
// method value. See TestGoInternalFunctionsMakeUnmarkedPlatformCalls in goc for
// the IR this is derived from.
type calleeConventions map[string]ir.CallConvention

// newCalleeConventions indexes a module's functions by symbol.
func newCalleeConventions(m *ir.Module) calleeConventions {
	c := make(calleeConventions, len(m.Funcs))
	for _, f := range m.Funcs {
		c[f.Name] = f.CallConv
	}
	return c
}

// forCall returns the convention to lower one call instruction against.
//
// The order of preference is: an explicit convention on the call instruction
// (goc sets this on every closure call, which is the only way an ABIInternal
// function is reached); then the callee's own convention when the call names a
// symbol this module defines; then the platform ABI. The final fallback covers
// calls to symbols defined elsewhere -- C runtime helpers, other objects -- which
// are platform ABI by definition, since ABIInternal is only ever produced within
// a module cg12 compiled.
func (c calleeConventions) forCall(f *ir.Func, call *ir.Instr) ir.CallConvention {
	if call.CallConvSet {
		return call.CallConv
	}
	if len(call.Args) > 0 {
		if sym, ok := calleeSymbolOf(f, call.Args[0]); ok {
			if cc, known := c[sym]; known {
				return cc
			}
		}
	}
	return ir.CallConvPlatform
}

// callIsGoInternal reports whether a *lowered* call is a Go ABIInternal one.
//
// It reads the convention lowerCalls recorded on the instruction rather than
// resolving it again, because by emission time the callee resolution is done and
// the instruction is the record of it. Every call lowerCalls emits sets
// CallConvSet, so an unset one is an unlowered call and answers the platform ABI,
// which is what it would have been lowered as.
func callIsGoInternal(call *ir.Instr) bool {
	return call.CallConvSet && call.CallConv == ir.CallConvGoInternal
}

// calleeSymbolOf returns the symbol a direct call targets. An indirect call
// resolves its callee through a temporary and returns false; such a call carries
// its convention on the instruction instead.
func calleeSymbolOf(f *ir.Func, ref ir.Ref) (string, bool) {
	if ref.Kind != ir.RefConst || int(ref.ID) >= len(f.Consts) {
		return "", false
	}
	k := f.Consts[ref.ID]
	if k.Kind != ir.ConstSym {
		return "", false
	}
	return k.Sym, true
}

// argLoc is where one argument is passed: a physical register or a stack slot at
// the given byte offset in the outgoing/incoming argument area.
type argLoc struct {
	reg     Reg
	onStack bool
	stacky  int
}

// argAssigner walks a sequence of argument classes and assigns each to a register
// or, once a bank is exhausted, to the stack. Integer and floating-point
// arguments consume independent register banks.
//
// It carries its own register tables rather than reading the package-level ones
// so a single walk is bound to one convention for its whole life; construct it
// with newArgAssigner.
type argAssigner struct {
	intRegs   []Reg
	floatRegs []Reg
	goABI     bool

	ngrn, nsrn int
	nsaa       int // next stacked-argument byte offset
}

// newArgAssigner returns an assigner bound to a convention.
func newArgAssigner(goABI bool) argAssigner {
	c := conventionABI(goInternalConvention(goABI))
	return argAssigner{intRegs: c.intArgRegs, floatRegs: c.floatArgRegs, goABI: c.packsStackArgs}
}

// newArgAssignerFor is newArgAssigner keyed by the convention itself, for the
// lowering sites that carry an ir.CallConvention rather than the older boolean.
func newArgAssignerFor(cc ir.CallConvention) argAssigner {
	return newArgAssigner(cc == ir.CallConvGoInternal)
}

func (a *argAssigner) assign(cls ir.Cls) argLoc {
	if cls.IsFloat() {
		if a.nsrn < len(a.floatRegs) {
			r := a.floatRegs[a.nsrn]
			a.nsrn++
			return argLoc{reg: r}
		}
	} else if a.ngrn < len(a.intRegs) {
		r := a.intRegs[a.ngrn]
		a.ngrn++
		return argLoc{reg: r}
	}
	if a.goABI {
		off := a.assignStack(cls.Size(), cls.Size())
		return argLoc{onStack: true, stacky: off}
	}
	off := a.nsaa
	a.nsaa += 8 // System V scalars occupy one 8-byte stack slot
	return argLoc{onStack: true, stacky: off}
}

// assignStack packs one stacked value at its natural alignment, the Go
// ABIInternal rule. System V's path in assign bypasses it: there every scalar
// takes a whole 8-byte slot regardless of size.
func (a *argAssigner) assignStack(size, alignment int) int {
	if alignment <= 0 {
		alignment = 1
	}
	offset := roundUp(a.nsaa, alignment)
	a.nsaa = offset + size
	return offset
}

// stackBytes returns the 16-aligned size of the stacked-argument area.
func (a *argAssigner) stackBytes() int { return roundUp(a.nsaa, 16) }

// retRegFor returns the register a scalar value of the given class is returned
// in under convention cc.
//
// The two conventions happen to agree for a single scalar -- System V's RAX/XMM0
// are also ABIInternal's first argument registers -- and that coincidence is why
// this is written as a table lookup rather than as the constants it resolves to
// today. They part company at the second result register (RDX vs RBX), which is
// where an aggregate return goes, so hard-coding the scalar answer would leave
// the two halves of the same rule in different places.
func retRegFor(cc ir.CallConvention, cls ir.Cls) Reg {
	c := conventionABI(cc)
	if cls.IsFloat() {
		return c.retFloatRegs[0]
	}
	return c.retIntRegs[0]
}

// retIntRegs / retSSERegs are the System V aggregate-return register sequences,
// reached through conventionABI(...).retIntRegs / .retFloatRegs.
var retIntRegs = []Reg{RAX, RDX}
var retSSERegs = []Reg{XMM0, XMM(1)}

// crossConventionClobbers returns the registers a call made under convention
// callee destroys that a body emitted under convention caller believes are
// preserved for it.
//
// It exists because the two questions amd64 has to answer about one call site
// come from different conventions. What the *body* may keep in a register across
// the call is decided by the caller's convention (callersave.go and gcalloc.go
// both read emissionConvention); what the *callee* actually preserves is decided
// by the callee's. Those agreed as long as every function was System V. They do
// not agree now: an ABIInternal callee preserves nothing at all, so a System V
// caller that parked a value in RBX or R12..R15 across a closure call would find
// it destroyed.
//
// The difference is expressed back into the IR as extra Defs on the lowered call
// (lowerCalls), which is the only vocabulary the allocator and the caller-save
// pass share for "this instruction destroys these registers" -- and it is the
// accurate statement, not a workaround: an ABIInternal call really does define
// those registers as far as the surrounding System V body is concerned.
//
// The reverse direction needs nothing. An ABIInternal *caller* already treats
// every register as clobbered by every call, which covers a platform-ABI callee
// as a special case.
func crossConventionClobbers(caller, callee ir.CallConvention) []Reg {
	if !conventionABI(caller).savesCalleeRegs || conventionABI(callee).savesCalleeRegs {
		return nil
	}
	var regs []Reg
	for r := RAX; r <= R15; r++ {
		if calleeSavedFor(caller, r) && !calleeSavedFor(callee, r) {
			regs = append(regs, r)
		}
	}
	return regs
}

// newPinned creates a fresh temporary hard-bound to physical register r.
func newPinned(f *ir.Func, r Reg, cls ir.Cls) ir.Ref {
	ref := f.NewTemp(fmt.Sprintf("R%d", int(r)), cls)
	t := f.Temp(ref)
	t.Fixed = true
	t.Reg = int(r)
	return ref
}
