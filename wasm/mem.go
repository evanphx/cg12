package wasm

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// wasmPageSize is the size of one WebAssembly linear-memory page.
const wasmPageSize = 1 << 16

// shadowStackBytes is the linear-memory region reserved above the data segment
// for the shadow stack that backs `alloc` (Wasm's operand stack is not
// addressable, so stack allocations live in linear memory).
const shadowStackBytes = 1 << 18 // 256 KiB

// nullGuard reserves the low addresses so that address 0 is never a valid
// object, keeping a null pointer distinguishable.
const nullGuard = 16

// modCtx is the module-wide layout shared by every function emission: the
// address assigned to each data symbol, the function-table indices, and the
// linear-memory shape.
type modCtx struct {
	syms     map[string]int64    // data symbol -> linear address
	funcIdx  map[string]int      // function name -> function-table index
	funcs    map[string]*ir.Func // function name -> definition (for variadic calls)
	needTab  bool                // any indirect call or address-taken function
	stackTop int64               // initial shadow-stack pointer (grows down)
	pages    int                 // linear-memory size in pages
	needMem  bool                // emit a (memory ...) at all
	hasSP    bool                // any function allocates -> emit the $__sp global
}

// layoutModule assigns every data symbol a linear address and sizes the memory.
// Data is packed low (above the null guard); the shadow stack sits above it.
func layoutModule(m *ir.Module) *modCtx {
	ctx := &modCtx{
		syms:    make(map[string]int64, len(m.Data)),
		funcIdx: make(map[string]int, len(m.Funcs)),
		funcs:   make(map[string]*ir.Func, len(m.Funcs)),
	}
	for i, f := range m.Funcs {
		ctx.funcIdx[f.Name] = i
		ctx.funcs[f.Name] = f
	}
	ctx.needTab = needsTable(m, ctx.funcIdx)
	cursor := int64(nullGuard)
	for _, d := range m.Data {
		align := int64(d.Align)
		if align <= 0 {
			align = 1
		}
		cursor = roundUp(cursor, align)
		ctx.syms[d.Name] = cursor
		cursor += dataSize(d)
	}
	// Symbols referenced but neither defined here nor named functions are
	// external globals (e.g. a C driver's `int a;`). Give each a zeroed slot —
	// linear memory starts zeroed, so no data segment is needed.
	for _, name := range externalSyms(m, ctx.syms, ctx.funcIdx) {
		cursor = roundUp(cursor, externalSlot)
		ctx.syms[name] = cursor
		cursor += externalSlot
	}
	usesMemcpy := false
	for _, f := range m.Funcs {
		if funcNeedsFrame(f, ctx.funcs) {
			ctx.hasSP = true
		}
		if f.RetAgg != nil {
			usesMemcpy = true // aggregate returns copy through linear memory
		}
	}
	// Any reserved data/external memory (cursor past the null guard), a shadow
	// stack, or an aggregate copy needs a linear memory.
	ctx.needMem = cursor > nullGuard || ctx.hasSP || usesMemcpy

	top := roundUp(cursor, 16)
	if ctx.hasSP {
		top += shadowStackBytes
	}
	ctx.stackTop = top
	pages := (top + wasmPageSize - 1) / wasmPageSize
	if pages < 1 {
		pages = 1
	}
	ctx.pages = int(pages)
	return ctx
}

// needsTable reports whether the module requires a function table: it does when
// any call is indirect (callee is a computed value) or any function is
// address-taken (named by a symbol used somewhere other than a direct callee).
func needsTable(m *ir.Module, funcIdx map[string]int) bool {
	isFuncSym := func(f *ir.Func, r ir.Ref) bool {
		if r.Kind != ir.RefConst || f.Consts[r.ID].Kind != ir.ConstSym {
			return false
		}
		_, ok := funcIdx[f.Consts[r.ID].Sym]
		return ok
	}
	for _, f := range m.Funcs {
		for _, b := range f.Blocks {
			for i := range b.Instrs {
				in := &b.Instrs[i]
				for k, a := range in.Args {
					if in.Op == ir.OCall && k == 0 {
						// The callee slot: a symbol here is a direct call, but a
						// computed value is an indirect call needing the table.
						if a.Kind != ir.RefConst || f.Consts[a.ID].Kind != ir.ConstSym {
							return true
						}
						continue
					}
					if isFuncSym(f, a) {
						return true // a function used as a value
					}
				}
			}
		}
	}
	return false
}

// externalSlot is the size reserved for an external global of unknown type —
// enough for any scalar (including a long/double), aligned generously.
const externalSlot = 16

// externalSyms returns, in stable order, the symbols referenced by any function
// that are neither defined data nor functions: external globals to reserve
// zeroed memory for.
func externalSyms(m *ir.Module, syms map[string]int64, funcIdx map[string]int) []string {
	seen := map[string]bool{}
	var out []string
	consider := func(f *ir.Func, r ir.Ref) {
		if r.Kind != ir.RefConst || f.Consts[r.ID].Kind != ir.ConstSym {
			return
		}
		name := f.Consts[r.ID].Sym
		if _, ok := syms[name]; ok {
			return
		}
		if _, ok := funcIdx[name]; ok {
			return
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, f := range m.Funcs {
		for _, b := range f.Blocks {
			for i := range b.Instrs {
				in := &b.Instrs[i]
				for k, a := range in.Args {
					if in.Op == ir.OCall && k == 0 {
						continue // a bare undefined callee is a link error, not a global
					}
					consider(f, a)
				}
			}
		}
	}
	return out
}

// funcNeedsFrame reports whether f needs a shadow-stack frame: it does when it
// allocates, calls a function that returns a by-value aggregate, or calls a
// variadic function (whose argument buffer lives in this frame).
func funcNeedsFrame(f *ir.Func, funcs map[string]*ir.Func) bool {
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op.IsAlloc() || (in.Op == ir.OCall && in.RetAgg != nil) {
				return true
			}
			if in.Op == ir.OCall {
				callee := in.Arg(0)
				if callee.Kind == ir.RefConst && f.Consts[callee.ID].Kind == ir.ConstSym {
					if cf := funcs[f.Consts[callee.ID].Sym]; cf != nil && cf.Variadic {
						return true
					}
				}
			}
		}
	}
	return false
}

// dataSize returns the total byte size of a data definition.
func dataSize(d *ir.Data) int64 {
	var n int64
	for _, it := range d.Items {
		n += itemSize(it)
	}
	return n
}

// itemSize returns the byte size of one data item.
func itemSize(it ir.DataItem) int64 {
	switch {
	case it.Zero > 0:
		return int64(it.Zero)
	case it.Str != "":
		return int64(len(it.Str))
	case it.Sym != "":
		return int64(ptrCls.Size()) // a stored pointer is register-width
	default:
		n := len(it.Ints)
		if len(it.Flts) > n {
			n = len(it.Flts)
		}
		return int64(n * it.Sub.Size())
	}
}

// emitTable declares a function table holding every module function at its
// index, so a function pointer is simply that index and call_indirect can reach
// it. Indices match modCtx.funcIdx (a function's position in m.Funcs).
func emitTable(sb *strings.Builder, m *ir.Module) {
	fmt.Fprintf(sb, "\t(table %d funcref)\n", len(m.Funcs))
	sb.WriteString("\t(elem (i32.const 0)")
	for _, f := range m.Funcs {
		fmt.Fprintf(sb, " $%s", sanitize(f.Name))
	}
	sb.WriteString(")\n")
}

// emitData writes a data segment for every data definition, resolving symbol
// references to their assigned addresses.
func emitData(sb *strings.Builder, m *ir.Module, ctx *modCtx) error {
	for _, d := range m.Data {
		bytes, err := encodeData(d, ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(sb, "\t(data (i32.const %d) \"%s\")\n", ctx.syms[d.Name], escapeBytes(bytes))
	}
	return nil
}

// encodeData serialises a data definition to its little-endian byte image.
func encodeData(d *ir.Data, ctx *modCtx) ([]byte, error) {
	var out []byte
	for _, it := range d.Items {
		switch {
		case it.Zero > 0:
			out = append(out, make([]byte, it.Zero)...)
		case it.Str != "":
			out = append(out, it.Str...)
		case it.Sym != "":
			addr, ok := ctx.syms[it.Sym]
			if !ok {
				return nil, fmt.Errorf("wasm: data references unknown symbol %q", it.Sym)
			}
			out = appendUint(out, uint64(addr+it.Off), ptrCls.Size())
		case it.Sub == ir.SubS:
			for _, f := range it.Flts {
				out = appendUint(out, uint64(math.Float32bits(float32(f))), 4)
			}
		case it.Sub == ir.SubD:
			for _, f := range it.Flts {
				out = appendUint(out, math.Float64bits(f), 8)
			}
		default:
			for _, v := range it.Ints {
				out = appendUint(out, uint64(v), it.Sub.Size())
			}
		}
	}
	return out, nil
}

// appendUint appends the low `size` bytes of v in little-endian order.
func appendUint(dst []byte, v uint64, size int) []byte {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	return append(dst, buf[:size]...)
}

// escapeBytes renders a byte slice as a WAT string body (every byte as \XX).
func escapeBytes(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		fmt.Fprintf(&sb, "\\%02x", c)
	}
	return sb.String()
}

// roundUp rounds n up to the next multiple of a (a > 0).
func roundUp(n, a int64) int64 {
	if a <= 0 {
		return n
	}
	return ((n + a - 1) / a) * a
}
