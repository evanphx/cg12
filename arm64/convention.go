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
	ir.CallConvAAPCS64: {
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
	return ir.CallConvAAPCS64
}

// stackLinkBytesFor returns the frame-chain link a call resolved to goInternal
// prepends to its stacked arguments (zero under AAPCS64), so callers add it
// unconditionally rather than branching on the convention.
func stackLinkBytesFor(goInternal bool) int {
	return conventionABI(goInternalConvention(goInternal)).stackLinkBytes
}
