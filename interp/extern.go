package interp

// ExternFunc models an external symbol the IR calls but does not define — a libc
// routine, or a frontend's runtime hook. It receives the interpreter (for memory
// and stdout) and the evaluated arguments, and returns the result.
type ExternFunc func(mc *Machine, args []Value) (Value, error)

// WithExtern registers (or overrides) an external symbol. Use it to model the
// libc surface a program needs beyond the built-in intrinsics.
func WithExtern(name string, fn ExternFunc) Option {
	return func(mc *Machine) {
		if mc.externs == nil {
			mc.externs = map[string]ExternFunc{}
		}
		mc.externs[name] = fn
	}
}

// defaultExterns are the intrinsics every Machine provides: a heap and the
// mem/exit/char primitives a small program needs. printf/vprintf (the formatter)
// arrive in Phase C.
func defaultExterns() map[string]ExternFunc {
	return map[string]ExternFunc{
		"malloc":  extMalloc,
		"calloc":  extCalloc,
		"free":    func(mc *Machine, a []Value) (Value, error) { return W(0), nil },
		"memcpy":  extMemcpy,
		"memmove": extMemcpy, // interpreter memcpy is already overlap-safe
		"memset":  extMemset,
		"exit":    func(mc *Machine, a []Value) (Value, error) { return Value{}, &ExitTrap{Code: int(argAt(a, 0).i64())} },
		"putchar": extPutchar,
		"puts":    extPuts,
	}
}

// heapAlloc reserves size bytes on the bump heap (16-aligned) and returns the
// address.
func (mc *Machine) heapAlloc(size uint64) uint64 {
	addr := roundUpU(mc.heapNext, 16)
	mc.heapNext = addr + roundUpU(size, 16)
	if mc.heapNext <= addr { // size 0
		mc.heapNext = addr + 16
	}
	mc.mem.mapRange(addr, maxU(size, 1))
	return addr
}

// readCString reads NUL-terminated bytes at addr.
func (mc *Machine) readCString(addr uint64) ([]byte, bool) {
	var out []byte
	for {
		b, ok := mc.mem.load(addr, 1)
		if !ok {
			return nil, false
		}
		if b == 0 {
			return out, true
		}
		out = append(out, byte(b))
		addr++
	}
}

func extMalloc(mc *Machine, a []Value) (Value, error) {
	return ptrVal(mc.heapAlloc(argAt(a, 0).u64())), nil
}

func extCalloc(mc *Machine, a []Value) (Value, error) {
	n, sz := argAt(a, 0).u64(), argAt(a, 1).u64()
	return ptrVal(mc.heapAlloc(n * sz)), nil // fresh pages read as zero
}

func extMemcpy(mc *Machine, a []Value) (Value, error) {
	dst, src, n := argAt(a, 0).u64(), argAt(a, 1).u64(), argAt(a, 2).u64()
	buf, ok := mc.mem.readBytes(src, int(n))
	if !ok {
		return Value{}, mc.trapf("memcpy: source [%#x,+%d) unmapped", src, n)
	}
	if !mc.mem.writeBytes(dst, buf) {
		return Value{}, mc.trapf("memcpy: destination [%#x,+%d) unmapped", dst, n)
	}
	return ptrVal(dst), nil
}

func extMemset(mc *Machine, a []Value) (Value, error) {
	dst, c, n := argAt(a, 0).u64(), byte(argAt(a, 1).u64()), argAt(a, 2).u64()
	buf := make([]byte, int(n))
	for i := range buf {
		buf[i] = c
	}
	if !mc.mem.writeBytes(dst, buf) {
		return Value{}, mc.trapf("memset: [%#x,+%d) unmapped", dst, n)
	}
	return ptrVal(dst), nil
}

func extPutchar(mc *Machine, a []Value) (Value, error) {
	c := byte(argAt(a, 0).u64())
	mc.stdout.Write([]byte{c})
	return W(int32(c)), nil
}

func extPuts(mc *Machine, a []Value) (Value, error) {
	s, ok := mc.readCString(argAt(a, 0).u64())
	if !ok {
		return Value{}, mc.trapf("puts: string [%#x] unmapped", argAt(a, 0).u64())
	}
	mc.stdout.Write(s)
	mc.stdout.Write([]byte{'\n'})
	return W(0), nil
}

func argAt(a []Value, i int) Value {
	if i < len(a) {
		return a[i]
	}
	return Value{}
}

func maxU(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
