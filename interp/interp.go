package interp

import (
	"fmt"
	"io"

	"github.com/evanphx/cg12/ir"
)

// Machine executes an ir.Module. Construct one with New, then drive an exported
// function with Call. A Machine is single-threaded and holds mutable execution
// state (the call stack, the stack pointer, and memory); do not share one across
// goroutines.
type Machine struct {
	mod   *ir.Module
	funcs map[string]*ir.Func

	mem      *memory
	syms     map[string]uint64   // symbol name -> address
	funcAt   map[uint64]*ir.Func // text descriptor -> function (indirect calls)
	externAt map[uint64]string   // text descriptor -> extern name
	externs  map[string]ExternFunc

	sp         uint64 // stack pointer, descends as frames allocate
	heapNext   uint64 // bump heap cursor
	commonNext uint64 // DefineExtern cursor
	textNext   uint64 // next text descriptor

	cur   *frame // currently executing frame, for trap locations
	depth int    // call-stack depth, for the recursion guard

	stdout io.Writer

	fuel        int64
	maxDepth    int
	divZeroZero bool
}

// Option configures a Machine.
type Option func(*Machine)

// WithStdout directs intrinsic output (putchar/puts/...) to w (default
// io.Discard).
func WithStdout(w io.Writer) Option { return func(mc *Machine) { mc.stdout = w } }

// WithFuel bounds total executed instructions; exceeding it traps. <= 0 disables.
func WithFuel(n int64) Option { return func(mc *Machine) { mc.fuel = n } }

// WithDivZeroYieldsZero makes integer div/rem by zero yield 0 (matching an
// AArch64 sdiv/udiv) instead of trapping.
func WithDivZeroYieldsZero() Option { return func(mc *Machine) { mc.divZeroZero = true } }

// New loads a module: it verifies structure, lays out globals into memory,
// assigns symbol addresses, and refuses (up front) any function that is not
// portable, pre-lowering IR the interpreter can execute.
func New(m *ir.Module, opts ...Option) (*Machine, error) {
	if err := ir.VerifyModule(m); err != nil {
		return nil, fmt.Errorf("interp: %w", err)
	}
	mc := &Machine{
		mod:        m,
		funcs:      make(map[string]*ir.Func, len(m.Funcs)),
		mem:        newMemory(),
		syms:       map[string]uint64{},
		funcAt:     map[uint64]*ir.Func{},
		externAt:   map[uint64]string{},
		stdout:     io.Discard,
		maxDepth:   100000,
		sp:         segStack,
		heapNext:   segHeap,
		commonNext: segCommon,
		textNext:   segText,
	}
	mc.externs = defaultExterns()
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
	// Text descriptors: a distinct address per function and extern, so a symbol
	// used as a value (a function pointer, a call through a variable) resolves and
	// round-trips through memory. These pages are never mapped, so treating a code
	// address as data faults.
	for _, f := range m.Funcs {
		mc.syms[f.Name] = mc.textNext
		mc.funcAt[mc.textNext] = f
		mc.textNext += 16
	}
	for name := range mc.externs {
		if _, ok := mc.syms[name]; ok {
			continue
		}
		mc.syms[name] = mc.textNext
		mc.externAt[mc.textNext] = name
		mc.textNext += 16
	}
	if err := mc.layoutGlobals(); err != nil {
		return nil, err
	}
	for _, f := range m.Funcs {
		if err := refuseFunc(f); err != nil {
			return nil, err
		}
	}
	return mc, nil
}

func (mc *Machine) symAddr(name string) (uint64, bool) {
	a, ok := mc.syms[name]
	return a, ok
}

// Call invokes the named function with the given arguments and returns its
// result (a zero Value for a void function).
func (mc *Machine) Call(name string, args ...Value) (Value, error) {
	f, ok := mc.funcs[name]
	if !ok {
		return Value{}, fmt.Errorf("interp: no function %q", name)
	}
	return mc.callFunc(f, args)
}

// --- harness helpers ---------------------------------------------------------

// GlobalAddr returns the address of a defined symbol (a global, function, or
// DefineExtern object).
func (mc *Machine) GlobalAddr(name string) (uint64, bool) { return mc.symAddr(name) }

// DefineExtern reserves a zeroed object of the given size and alignment and binds
// name to it. It models a symbol a C driver would define (e.g. `int a;`) so the
// IL under test can store to and load from it without that driver.
func (mc *Machine) DefineExtern(name string, size, align int) uint64 {
	if align <= 0 {
		align = 1
	}
	addr := roundUpU(mc.commonNext, uint64(align))
	mc.commonNext = addr + uint64(size)
	mc.mem.mapRange(addr, maxU(uint64(size), 1))
	mc.syms[name] = addr
	return addr
}

// NewCString allocates a NUL-terminated copy of s on the heap and returns its
// address (for building argv and string arguments).
func (mc *Machine) NewCString(s string) uint64 {
	addr := mc.heapAlloc(uint64(len(s) + 1))
	mc.mem.writeBytes(addr, append([]byte(s), 0))
	return addr
}

// Alloc reserves n zeroed heap bytes and returns the address.
func (mc *Machine) Alloc(n int) uint64 { return mc.heapAlloc(uint64(n)) }

// Store / Load give a harness direct memory access for setting up and checking
// arguments passed by pointer.
func (mc *Machine) Store(addr uint64, size int, bits uint64) error {
	if !mc.mem.store(addr, size, bits) {
		return fmt.Errorf("interp: store to unmapped %#x", addr)
	}
	return nil
}
func (mc *Machine) ReadBytes(addr uint64, n int) ([]byte, error) {
	b, ok := mc.mem.readBytes(addr, n)
	if !ok {
		return nil, fmt.Errorf("interp: read from unmapped %#x", addr)
	}
	return b, nil
}

// refuseFunc rejects a function containing anything the interpreter will not
// execute, naming the exact construct.
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
