package interp

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
)

// Trap is a runtime error raised by the interpreter: a defined-to-be-undefined
// operation (div by zero, hlt), a memory fault, an out-of-range control transfer,
// a call to an unmodelled symbol, or a resource limit. It is returned as an error
// from Call — never a Go panic and never a silently fabricated value — so a
// differential harness can report it as a comparison failure with a location.
type Trap struct {
	Fn    string // function the trap occurred in
	Block string // block name
	Index int    // instruction index within the block, or -1 for a terminator
	Op    string // op or terminator description
	Msg   string
}

func (t *Trap) Error() string {
	where := t.Fn
	if t.Block != "" {
		where += ":" + t.Block
		if t.Index >= 0 {
			where += fmt.Sprintf("[%d]", t.Index)
		}
	}
	if t.Op != "" {
		where += " " + t.Op
	}
	return fmt.Sprintf("interp trap in %s: %s", where, t.Msg)
}

// ExitTrap is the distinguished error the exit intrinsic raises; a harness reads
// Code rather than treating it as a failure.
type ExitTrap struct{ Code int }

func (e *ExitTrap) Error() string { return fmt.Sprintf("interp: exit(%d)", e.Code) }

// trapf raises a Trap located at the machine's current execution point.
func (mc *Machine) trapf(format string, a ...any) *Trap {
	t := &Trap{Msg: fmt.Sprintf(format, a...)}
	if mc.cur != nil {
		t.Fn = mc.cur.fn.Name
		if mc.cur.block != nil {
			t.Block = mc.cur.block.Name
		}
		t.Index = mc.cur.index
		t.Op = mc.cur.opDesc
	}
	return t
}

// loadErr is a load-time (New) rejection: it names the function, block, and op
// that the interpreter refuses to execute. It is an ordinary error, not a Trap
// (nothing has run yet).
func loadErr(fn, block string, op ir.Op, msg string) error {
	return fmt.Errorf("interp: cannot load %s:%s: %s (%s)", fn, block, msg, op)
}
