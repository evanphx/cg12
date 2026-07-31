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
		packsStackArgs:  false,
		savesCalleeRegs: true,
		stackLinkBytes:  0,
	},
	ir.CallConvGoInternal: {
		intArgRegs:      goArgGP,
		floatArgRegs:    goArgFP,
		packsStackArgs:  true,
		savesCalleeRegs: false,
		stackLinkBytes:  0,
	},
}

// conventionABI returns the descriptor for a calling convention.
func conventionABI(cc ir.CallConvention) abiConvention { return abiConventions[cc] }

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

// retReg returns the register a value of the given class is returned in.
func retReg(cls ir.Cls) Reg {
	if cls.IsFloat() {
		return XMM0
	}
	return RAX
}

// retIntRegs / retSSERegs are the aggregate-return register sequences.
var retIntRegs = []Reg{RAX, RDX}
var retSSERegs = []Reg{XMM0, XMM(1)}

// newPinned creates a fresh temporary hard-bound to physical register r.
func newPinned(f *ir.Func, r Reg, cls ir.Cls) ir.Ref {
	ref := f.NewTemp(fmt.Sprintf("R%d", int(r)), cls)
	t := f.Temp(ref)
	t.Fixed = true
	t.Reg = int(r)
	return ref
}
