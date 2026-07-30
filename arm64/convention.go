package arm64

import "github.com/evanphx/cg12/ir"

// abiConvention captures the register and stack-layout rules that vary with a
// function's calling convention -- the ABI axis. It is deliberately orthogonal
// to the frame axis (managed vs fixed stack, carried by ir.Func.UsesManagedFrame),
// which governs the morestack prologue, the outgoing-argument area, and GC
// marking. Collecting the per-convention constants in one table keeps the
// backend's convention decisions a single lookup rather than scattered
// `if goInternal { ... } else { ... }` ladders with repeated magic numbers.
type abiConvention struct {
	// intArgRegs and floatArgRegs count the integer (x0..) and floating-point
	// (v0..) argument registers the convention passes values in.
	intArgRegs   int
	floatArgRegs int
	// packsStackArgs tightly packs stacked arguments by their natural size (Go
	// ABIInternal); otherwise each scalar occupies one 8-byte slot (AAPCS64).
	packsStackArgs bool
	// savesCalleeRegs means a callee preserves the AAPCS64 callee-saved set
	// (X19-X28 and the low 64 bits of V8-V15). Go ABIInternal preserves nothing.
	savesCalleeRegs bool
	// stackLinkBytes is the frame-chain link reserved ahead of the stacked
	// arguments; Go ABIInternal threads the caller's frame pointer through it.
	stackLinkBytes int
}

// abiConventions is indexed by ir.CallConvention.
var abiConventions = [...]abiConvention{
	ir.CallConvPlatform: {
		intArgRegs:      8,
		floatArgRegs:    8,
		packsStackArgs:  false,
		savesCalleeRegs: true,
		stackLinkBytes:  0,
	},
	ir.CallConvGoInternal: {
		intArgRegs:      16,
		floatArgRegs:    16,
		packsStackArgs:  true,
		savesCalleeRegs: false,
		stackLinkBytes:  goStackLinkSize,
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

// stackLinkBytesFor returns the frame-chain link a call resolved to goInternal
// prepends to its stacked arguments (zero under AAPCS64), so callers add it
// unconditionally rather than branching on the convention.
func stackLinkBytesFor(goInternal bool) int {
	return conventionABI(goInternalConvention(goInternal)).stackLinkBytes
}

// calleeConventions resolves the convention each call site must be lowered
// against. It is built once per object because a direct call names its callee by
// symbol, and the callee's convention is a property of the other function.
//
// The rule this replaces inferred an unmarked call's convention from the
// *enclosing* function, which is unsound: goc marks method-value wrappers,
// function literals, and funcvalue adapters CallConvGoInternal, and those
// functions make ordinary unmarked direct calls to plain platform-ABI functions
// (see TestGoInternalFunctionsMakeUnmarkedPlatformCalls in goc for that IR).
// Inheriting there lowers the call as ABIInternal against an AAPCS64 callee.
//
// arm64 hid the bug for a long time because both of its conventions assign
// integer arguments from X0 upward, so the two lowerings agree for <= 8 integer
// arguments. They diverge past the AAPCS64 register limit, in packsStackArgs
// (ABIInternal packs stacked arguments by natural size, AAPCS64 gives each
// scalar an 8-byte slot), and in stackLinkBytes. amd64 has no overlap at all --
// System V starts at RDI, ABIInternal at RAX -- so the same rule miscompiled the
// first method value it met; amd64/convention.go carries the identical rule.
type calleeConventions map[string]ir.CallConvention

// newCalleeConventions indexes by symbol the functions an object defines.
//
// It takes the function list rather than the *ir.Module its amd64 counterpart
// takes because an arm64 object also contains the functions lowered from the
// module's Plan 9 assembly, which are defined here just as much as m.Funcs are.
func newCalleeConventions(functions []*ir.Func) calleeConventions {
	c := make(calleeConventions, len(functions))
	for _, f := range functions {
		if f == nil {
			continue
		}
		c[f.Name] = f.CallConv
	}
	return c
}

// forCall returns the convention to lower one call instruction against.
//
// The order of preference is: an explicit convention on the call instruction
// (goc sets this on every closure call, which is the only way an ABIInternal
// function is reached, and applyAssemblyCallConventions sets it on every direct
// call whose callee this object defines); then the callee's own convention when
// the call names a symbol this object defines; then the platform ABI. The final
// fallback covers calls to symbols defined elsewhere -- C runtime helpers, other
// objects -- which are platform ABI by definition, since ABIInternal is only
// ever produced within a module cg12 compiled.
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

// goInternalCall reports whether one call must be lowered and emitted as
// ABIInternal. Every site that has to know -- argument assignment in lowering,
// the stack-argument offsets and frame handling in the emitter, and the
// outgoing-area size in the frame layout -- goes through here, so they cannot
// disagree about a call.
func (c calleeConventions) goInternalCall(f *ir.Func, call *ir.Instr) bool {
	return c.forCall(f, call) == ir.CallConvGoInternal
}

// loweredCallConvention returns the convention one already-lowered call was
// lowered against.
//
// lowerCalls stamps CallConv/CallConvSet on every OCall it rewrites, so after
// lowering the instruction is self-describing and the post-lowering passes --
// register allocation and the caller-save pass -- need no convention index to ask
// what a call does. It agrees with calleeConventions.forCall by construction,
// because forCall prefers exactly this field when it is set. An unstamped call
// has not been through lowering; the platform ABI is the same answer forCall
// gives such a call.
func loweredCallConvention(call *ir.Instr) ir.CallConvention {
	if call.CallConvSet {
		return call.CallConv
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

// moduleConventions builds the callee-convention map for the module that owns f.
// The compiler driver builds the map once for a whole object; this is for the
// paths (chiefly tests) that hold a single function and no assembly bundle.
func moduleConventions(f *ir.Func) calleeConventions {
	m := f.Module()
	if m == nil {
		return nil
	}
	return newCalleeConventions(m.Funcs)
}
