package interp

import (
	"strings"

	"github.com/evanphx/cg12/ir"
)

// atomicOp executes a named atomic intrinsic and is shared by both engines. The
// interpreter is single-threaded, so atomicity is inherent: each operation is an
// ordinary load / modify / store that nothing can interleave with. args are the
// resolved operands; for every atomic but the fence, args[0] is the address. It
// returns the result (valid when hasResult) and any memory trap.
func (mc *Machine) atomicOp(name string, args []Value) (res Value, hasResult bool, err error) {
	if name == "atomic.fence" {
		return Value{}, false, nil // a barrier; nothing to order in one thread
	}
	base, size, cls := atomicKind(name)
	addr := args[0].u64()

	load := func() (uint64, error) { return mc.memLoad(addr, size) }
	store := func(v uint64) error {
		if !mc.mem.store(addr, size, v) {
			return mc.trapf("atomic %s: store %d bytes at %#x: unmapped", base, size, addr)
		}
		return nil
	}
	// Compare only the accessed width, so a sub-word CAS ignores the high bits of
	// its operands.
	mask := ^uint64(0)
	if size < 8 {
		mask = uint64(1)<<(uint(size)*8) - 1
	}

	switch base {
	case "load":
		v, err := load()
		return intVal(cls, int64(v)), true, err
	case "store":
		return Value{}, false, store(args[1].u64())
	case "xchg":
		old, err := load()
		if err != nil {
			return Value{}, false, err
		}
		return intVal(cls, int64(old)), true, store(args[1].u64())
	case "cas":
		old, err := load()
		if err != nil {
			return Value{}, false, err
		}
		if old&mask == args[1].u64()&mask {
			if err := store(args[2].u64()); err != nil {
				return Value{}, false, err
			}
		}
		return intVal(cls, int64(old)), true, nil
	case "add", "sub", "and", "or", "xor":
		old, err := load()
		if err != nil {
			return Value{}, false, err
		}
		return intVal(cls, int64(old)), true, store(atomicRMW(base, old, args[1].u64()))
	}
	return Value{}, false, mc.trapf("interp: unknown atomic intrinsic %q", name)
}

// atomicKind splits an atomic intrinsic name into its base operation, access
// width in bytes, and result class: "atomic.add.l" -> ("add", 8, ClsL),
// "atomic.and.b" -> ("and", 1, ClsW). Sub-word results live in a word.
func atomicKind(name string) (base string, size int, cls ir.Cls) {
	s := strings.TrimPrefix(name, "atomic.")
	switch {
	case strings.HasSuffix(s, ".b"):
		return strings.TrimSuffix(s, ".b"), 1, ir.ClsW
	case strings.HasSuffix(s, ".h"):
		return strings.TrimSuffix(s, ".h"), 2, ir.ClsW
	case strings.HasSuffix(s, ".l"):
		return strings.TrimSuffix(s, ".l"), 8, ir.ClsL
	default:
		return strings.TrimSuffix(s, ".w"), 4, ir.ClsW
	}
}

// atomicRMW computes the new value a read-modify-write stores. The store truncates
// to the access width, so no masking is needed here.
func atomicRMW(op string, old, val uint64) uint64 {
	switch op {
	case "add":
		return old + val
	case "sub":
		return old - val
	case "and":
		return old & val
	case "or":
		return old | val
	case "xor":
		return old ^ val
	}
	return old
}
