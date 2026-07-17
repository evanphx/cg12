package interp

import (
	"fmt"
	"io"

	"github.com/evanphx/cg12/ir"
)

// Machine executes an ir.Module. Construct one with New, then drive an exported
// function with Call. A Machine is single-threaded and holds mutable execution
// state (the call-stack pointer and, from Phase B, memory); do not share one
// across goroutines.
type Machine struct {
	mod   *ir.Module
	funcs map[string]*ir.Func

	cur   *frame // currently executing frame, for trap locations
	depth int    // call-stack depth, for the recursion guard

	stdout io.Writer

	// limits and options
	fuel        int64 // remaining instruction budget; <= 0 means unlimited
	maxDepth    int
	divZeroZero bool // arm64-equivalent: div/rem by zero yields 0 instead of trapping
}

// Option configures a Machine.
type Option func(*Machine)

// WithStdout directs intrinsic output (printf/puts/...) to w. Defaults to
// io.Discard.
func WithStdout(w io.Writer) Option { return func(mc *Machine) { mc.stdout = w } }

// WithFuel bounds total executed instructions; exceeding it is a trap. <= 0
// disables the limit. Essential for fuzzing.
func WithFuel(n int64) Option { return func(mc *Machine) { mc.fuel = n } }

// WithDivZeroYieldsZero makes integer div/rem by zero yield 0 (matching an
// AArch64 sdiv/udiv, which does not fault) instead of trapping. The default is
// to trap, because opt/fold.go deliberately leaves div-by-zero undefined and a
// differential corpus that divides by zero is a bug worth surfacing.
func WithDivZeroYieldsZero() Option { return func(mc *Machine) { mc.divZeroZero = true } }

// New loads a module: it verifies structure, then refuses (up front, not
// mid-run) any function that is not portable, pre-lowering IR the interpreter can
// execute. On success the module's functions are ready to Call.
func New(m *ir.Module, opts ...Option) (*Machine, error) {
	if err := ir.VerifyModule(m); err != nil {
		return nil, fmt.Errorf("interp: %w", err)
	}
	mc := &Machine{
		mod:      m,
		funcs:    make(map[string]*ir.Func, len(m.Funcs)),
		stdout:   io.Discard,
		maxDepth: 100000,
	}
	for _, o := range opts {
		o(mc)
	}
	for _, f := range m.Funcs {
		if lf := f.LoweredFor(); lf != "" {
			return nil, fmt.Errorf("interp: %s is lowered for %s; the interpreter runs pre-lowering IR", f.Name, lf)
		}
		if _, dup := mc.funcs[f.Name]; dup {
			return nil, fmt.Errorf("interp: duplicate function %q", f.Name)
		}
		mc.funcs[f.Name] = f
	}
	for _, f := range m.Funcs {
		if err := refuseFunc(f); err != nil {
			return nil, err
		}
	}
	return mc, nil
}

// refuseFunc rejects a function containing anything the interpreter will not
// execute, naming the exact construct. Rejecting here means a program either
// loads clean or fails with a clear reason, never a surprise mid-run.
func refuseFunc(f *ir.Func) error {
	for _, t := range f.Temps {
		if t.Cls == ir.ClsQ {
			return fmt.Errorf("interp: %s: temp %%%s is a 128-bit quad, which is unsupported", f.Name, t.Name)
		}
	}
	for i := range f.Consts {
		c := &f.Consts[i]
		if c.Cls == ir.ClsQ {
			return fmt.Errorf("interp: %s: a 128-bit quad constant is unsupported", f.Name)
		}
		if c.Kind == ir.ConstSym && c.Thread {
			return fmt.Errorf("interp: %s: thread-local symbol %q is unsupported", f.Name, c.Sym)
		}
	}
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Cls == ir.ClsQ {
				return loadErr(f.Name, b.Name, in.Op, "128-bit quad result")
			}
			if len(in.Defs) != 0 {
				return loadErr(f.Name, b.Name, in.Op, "multi-register (Defs) result — lowering artifact")
			}
			if in.AggArgs != nil || in.RetAgg != nil {
				return loadErr(f.Name, b.Name, in.Op, "aggregate-by-value call — not yet supported (Phase C)")
			}
			if msg, bad := refusedOp(in.Op); bad {
				return loadErr(f.Name, b.Name, in.Op, msg)
			}
		}
		if len(b.Jmp.Args) != 0 {
			return loadErr(f.Name, b.Name, ir.ONop, "multi-value terminator (Jmp.Args) — lowering artifact")
		}
	}
	return nil
}

// refusedOp reports whether an op has no portable interpreter meaning.
func refusedOp(op ir.Op) (string, bool) {
	switch op {
	case ir.OAsm:
		return "inline assembly", true
	case ir.OGetReg, ir.OSetReg:
		return "machine-register access", true
	case ir.OThreadPtr, ir.OTLSOffset, ir.OTLSIndexAddr:
		return "thread-local access", true
	case ir.ORotr, ir.OBic:
		return "backend-private op (lowered IR)", true
	case ir.OArg, ir.OPar, ir.OParEnv:
		return "ABI-lowering artifact (lowered IR)", true
	}
	return "", false
}

// Call invokes the named exported function with the given arguments and returns
// its result (a zero Value for a void function). Extra arguments beyond the
// declared parameters are allowed only for a variadic function.
func (mc *Machine) Call(name string, args ...Value) (Value, error) {
	f, ok := mc.funcs[name]
	if !ok {
		return Value{}, fmt.Errorf("interp: no function %q", name)
	}
	return mc.callFunc(f, args)
}
