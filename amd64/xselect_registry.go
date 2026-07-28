package amd64

import "github.com/evanphx/cg12/ir"

// xselector is one operation family's attempt at an instruction: it emits `in`
// and reports true, or recognizes nothing and reports false so the next family
// gets a look. It is a plain function rather than a method so that each family can
// define its own in its own file.
type xselector func(s *xsel, in *ir.Instr) bool

// selectors is the probe chain selectInt walks before falling back to selectCore,
// which holds the ops the backend started with. One entry per operation family,
// each implemented in the xselect_<family>.go file named beside it.
//
// This is a fixed literal and not init()-based self-registration, on purpose.
// Registration from init() would spare this one line per family, but the resulting
// chain order would be the order the Go toolchain happens to initialize files in
// -- unwritten anywhere, invisible at the call site, and silently reordered by
// renaming a file. Order is load-bearing as soon as two families can claim the
// same op: the immediates work (Track A, agent A3) intercepts ir.OAdd and ir.OCmp
// with a constant operand that selectCore would otherwise handle generically, so
// it must probe first. A selection-order bug is among the least pleasant kinds to
// debug -- the code emitted is valid, just not the code intended -- so the order
// is spelled out here where it can be read and reviewed.
//
// Two rules for anything added here:
//
//   - selectCore always runs last, so a family that claims an op selectCore also
//     handles is overriding it. That is a legitimate thing to do (it is how a
//     better encoding for a special case gets chosen) but it must be deliberate.
//   - first match wins, so families claiming disjoint ops may go in any order.
//     Place an overriding family above the entries it must beat.
var selectors = []xselector{
	selectAtomic,
	selectBits,
	selectWide,
	selectTLS,
}
