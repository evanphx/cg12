package arm64

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// CompileObject lowers, allocates, and emits every function and data definition
// in m directly to an ELF relocatable object — machine-code bytes, no external
// assembler. It handles integers, floats, direct/indirect/tail calls, variadics,
// aggregates, and data with relocations — and, when the
// module carries source positions, emits DWARF line info (.debug_line/
// .debug_info/.debug_abbrev) directly, no assembler required.
// Options configures object emission.
type Options struct {
	// GC, when set, emits garbage-collector support code (e.g. safepoint polls)
	// late during emission, keeping it out of the normal generated code.
	GC GCStrategy

	// TLSModel selects how a thread-local's address is computed. The default,
	// local-exec, only works in the main executable, where the offset from the
	// thread pointer is fixed at link time. Initial-exec reads that offset from a
	// GOT slot the loader fills, so it also works in a shared library.
	TLSModel TLSModel
}

// TLSModel names a way of reaching a thread-local variable.
type TLSModel uint8

const (
	// TLSLocalExec bakes the offset from the thread pointer into the code. Only
	// the main executable can do this: its block sits at a fixed place in every
	// thread.
	TLSLocalExec TLSModel = iota

	// TLSInitialExec reads the offset from a GOT slot the loader fills, so the
	// variable may live in a shared library. It still assumes the library is there
	// from the start -- a library brought in later by dlopen may find no room left
	// in the static block, which is what general-dynamic exists for.
	TLSInitialExec

	// TLSGeneralDynamic asks __tls_get_addr for the address, which finds or
	// allocates this thread's block for the owning module. It works for any
	// thread-local, including one in a library brought in by dlopen, at the cost of
	// a call.
	TLSGeneralDynamic
)

// CompileObject emits an ELF relocatable object for m with default options.
func CompileObject(m *ir.Module) ([]byte, error) {
	return CompileObjectWith(m, Options{})
}

// CompileObjectWith emits an ELF relocatable object for m, applying opts (such as
// a GC strategy).
func CompileObjectWith(m *ir.Module, opts Options) ([]byte, error) {
	o, err := CompileToObjectWith(m, opts)
	if err != nil {
		return nil, err
	}
	return o.MarshalELF()
}

// CompileToObject builds the in-memory relocatable object for m (without
// serializing it to ELF). It is how the linker ingests an IR module: the result
// is the same Object model that ReadELF produces for an on-disk .o.
func CompileToObject(m *ir.Module) (*obj.Object, error) {
	return CompileToObjectWith(m, Options{})
}

// CompileToObjectWith is CompileToObject with options (e.g. a GC strategy).
func CompileToObjectWith(m *ir.Module, opts Options) (*obj.Object, error) {
	o := &obj.Object{Machine: obj.EM_AARCH64}
	var rows []obj.LineRow
	var dfuncs []obj.DwarfFunc
	var smFuncs []stackMapFunc
	anchor := ""
	for _, f := range m.Funcs {
		name := sanitize(f.Name)
		params := dwarfParams(f)
		paramTemps := paramTempIDs(f)
		ir.LowerPointers(f, ptrCls)
		if err := lower(f, opts.TLSModel); err != nil {
			return nil, fmt.Errorf("function %s: %w", f.Name, err)
		}
		alloc, err := regAlloc(f)
		if err != nil {
			return nil, fmt.Errorf("function %s: %w", f.Name, err)
		}
		mc, err := emitMachine(f, alloc, opts.GC, opts.TLSModel)
		if err != nil {
			return nil, fmt.Errorf("function %s: %w", f.Name, err)
		}
		base := uint64(len(o.Text))
		if anchor == "" {
			anchor = name
		}
		o.Text = append(o.Text, mc.code...)
		o.Syms = append(o.Syms, obj.Sym{
			Name: name, Section: obj.SecText, Value: base,
			Size: uint64(len(mc.code)), Global: f.Linkage.Export, Func: true,
		})
		// Local symbols at address-taken blocks (&&label used in static data).
		for _, bs := range mc.blockSyms {
			o.Syms = append(o.Syms, obj.Sym{
				Name: bs.name, Section: obj.SecText, Value: base + uint64(bs.off),
			})
		}
		for _, rl := range mc.relocs {
			rl.Offset += base
			o.Relocs = append(o.Relocs, rl)
		}
		// Attach each parameter's DWARF location, computed from its binding.
		for i := range params {
			params[i].Loc = mc.m.varLoc(paramTemps[i])
		}
		df := obj.DwarfFunc{
			Name: name, Sym: name, Size: uint64(len(mc.code)),
			HasRet: f.HasRet, RetType: dwarfRetType(f), Params: params, External: f.Linkage.Export,
			Inlines: buildInlineTree(mc.inl, uint64(len(mc.code))),
		}
		if len(mc.rows) > 0 {
			df.DeclFile, df.DeclLine = mc.rows[0].File, mc.rows[0].Line
		}
		dfuncs = append(dfuncs, df)
		for _, r := range mc.rows {
			r.TextOff += base
			rows = append(rows, r)
		}
		if len(mc.safepoints) > 0 {
			smFuncs = append(smFuncs, stackMapFunc{sym: name, points: mc.safepoints})
		}
	}
	for _, d := range m.Data {
		if err := addData(o, d); err != nil {
			return nil, fmt.Errorf("data %s: %w", d.Name, err)
		}
	}
	// Emit DWARF when the module carries a source-file table.
	if len(m.Files) > 0 && anchor != "" {
		o.SetDWARF(m.Files, rows, dfuncs, uint64(len(o.Text)), anchor, "cg12", ".", m.Files[0], obj.R_AARCH64_ABS64, 0x6d) // DW_OP_reg29 (x29)
	}
	// Emit GC stack maps when any safepoint carries a live root.
	if len(smFuncs) > 0 {
		setStackMap(o, smFuncs)
	}
	return o, nil
}

// paramTempIDs returns the temporary id of each parameter, in order, captured
// before lowering (the ids are stable across it).
func paramTempIDs(f *ir.Func) []int {
	ids := make([]int, len(f.Params))
	for i, p := range f.Params {
		ids[i] = p.ID
	}
	return ids
}

// stackMapFunc pairs a function symbol with its safepoints for the stack-map
// section builder.
type stackMapFunc struct {
	sym    string
	points []safepoint
}

// setStackMap builds the .cg12_stackmaps section: a self-describing table of
// safepoint return addresses (each an ABS64 relocation to a function symbol plus
// the return offset) and the live GC-root locations at each. A garbage collector
// finds it via the __cg12_stackmaps symbol.
//
// Format (little-endian):
//
//	magic  "SMAP"                  (4 bytes)
//	version u16 = 2, reserved u16
//	count   u32                    (number of safepoints)
//	repeated count times:
//	  pc     u64                   (return address; relocated)
//	  nroots u32
//	  repeated nroots times:
//	    kind u8 (0=register, 1=frame), reserved u8[3], value i32, type u32
func setStackMap(o *obj.Object, funcs []stackMapFunc) {
	var b []byte
	put16 := func(v uint16) { b = append(b, byte(v), byte(v>>8)) }
	put32 := func(v uint32) { b = append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24)) }
	put64 := func(v uint64) {
		for i := 0; i < 8; i++ {
			b = append(b, byte(v>>(8*i)))
		}
	}

	total := 0
	for _, f := range funcs {
		total += len(f.points)
	}
	b = append(b, 'S', 'M', 'A', 'P')
	put16(2)
	put16(0)
	put32(uint32(total))
	for _, f := range funcs {
		for _, sp := range f.points {
			o.StackMapRelocs = append(o.StackMapRelocs, obj.Reloc{
				Offset: uint64(len(b)), Sym: f.sym, Type: obj.R_AARCH64_ABS64, Addend: int64(sp.pc),
			})
			put64(0) // pc, filled by the relocation
			put32(uint32(len(sp.roots)))
			for _, r := range sp.roots {
				b = append(b, r.kind, 0, 0, 0)
				put32(uint32(r.val))
				put32(r.typ)
			}
		}
	}
	o.StackMap = b
	o.Syms = append(o.Syms, obj.Sym{
		Name: "__cg12_stackmaps", Section: obj.SecStackMap, Value: 0, Size: uint64(len(b)), Global: true,
	})
}

// dwarfType maps an IR class to a DWARF base type. DW_ATE_signed = 0x05,
// DW_ATE_float = 0x04.
func dwarfType(cls ir.Cls) obj.DwarfType {
	switch cls {
	case ir.ClsW:
		return obj.DwarfType{Name: "int", Encoding: 0x05, Size: 4}
	case ir.ClsS:
		return obj.DwarfType{Name: "float", Encoding: 0x04, Size: 4}
	case ir.ClsD:
		return obj.DwarfType{Name: "double", Encoding: 0x04, Size: 8}
	default: // ClsL and lowered pointers
		return obj.DwarfType{Name: "long", Encoding: 0x05, Size: 8}
	}
}

func dwarfRetType(f *ir.Func) obj.DwarfType {
	if f.RetAgg != nil {
		return dwarfType(ir.ClsL) // returned by reference: an address
	}
	return dwarfType(f.Retty)
}

// dwarfParams captures parameter names and types before lowering mutates the
// function. An aggregate-by-value parameter is a pointer (class L).
func dwarfParams(f *ir.Func) []obj.DwarfParam {
	var ps []obj.DwarfParam
	for _, p := range f.Params {
		cls := p.Cls
		if p.Agg != nil {
			cls = ir.ClsL
		}
		ps = append(ps, obj.DwarfParam{Name: p.Name, Type: dwarfType(cls)})
	}
	return ps
}

// inlineNode is an inlined region under construction, before conversion to the
// flat obj.InlineRange tree.
type inlineNode struct {
	site     *ir.InlineSite
	lo, hi   uint64
	children []*inlineNode
}

// buildInlineTree turns a function's inline-context samples into a nested tree of
// inlined-subroutine ranges. Samples mark the offset at which the context
// changes; the final context runs to size. Because inline contexts are interned
// during inlining, sites can be compared by pointer.
func buildInlineTree(samples []inlSample, size uint64) []obj.InlineRange {
	if len(samples) == 0 {
		return nil
	}
	chainOf := func(s *ir.InlineSite) []*ir.InlineSite {
		var c []*ir.InlineSite
		for ; s != nil; s = s.Parent {
			c = append(c, s)
		}
		for i, j := 0, len(c)-1; i < j; i, j = i+1, j-1 {
			c[i], c[j] = c[j], c[i] // reverse to outermost-first
		}
		return c
	}

	var roots, stack []*inlineNode
	addChild := func(n *inlineNode) {
		if len(stack) == 0 {
			roots = append(roots, n)
		} else {
			top := stack[len(stack)-1]
			top.children = append(top.children, n)
		}
	}
	for _, s := range samples {
		chain := chainOf(s.site)
		k := 0
		for k < len(stack) && k < len(chain) && stack[k].site == chain[k] {
			k++
		}
		for len(stack) > k { // close contexts we left
			top := stack[len(stack)-1]
			top.hi = s.off
			stack = stack[:len(stack)-1]
		}
		for _, site := range chain[k:] { // open contexts we entered
			n := &inlineNode{site: site, lo: s.off}
			addChild(n)
			stack = append(stack, n)
		}
	}
	for len(stack) > 0 { // the final context runs to the end of the function
		top := stack[len(stack)-1]
		top.hi = size
		stack = stack[:len(stack)-1]
	}
	return convInlineNodes(roots)
}

func convInlineNodes(ns []*inlineNode) []obj.InlineRange {
	var out []obj.InlineRange
	for _, n := range ns {
		out = append(out, obj.InlineRange{
			Callee:   sanitize(n.site.Callee),
			CallFile: int(n.site.Call.File),
			CallLine: int(n.site.Call.Line),
			Lo:       n.lo, Hi: n.hi,
			Children: convInlineNodes(n.children),
		})
	}
	return out
}

// addData appends a data definition to the object's .data section.
func addData(o *obj.Object, d *ir.Data) error {
	// Nothing but zero fill: it needs no bytes in the file, only room at run time.
	// .bss follows .data in memory and .tbss follows .tdata in each thread's block,
	// so the symbol's value counts from the start of its own zero-filled region.
	if allZero(d) {
		size, sec, at := zeroSize(d), obj.SecBss, &o.BssSize
		align := &o.BssAlign
		if d.Linkage.Thread {
			sec, at, align = obj.SecTbss, &o.TbssSize, &o.TlsAlign
		}
		// The datum is padded to its alignment within the section; the section
		// itself has to be placed at least that well, or the padding aligns it
		// relative to nothing.
		if d.Align > *align {
			*align = d.Align
		}
		if d.Align > 0 {
			*at = obj.AlignInt(*at, d.Align)
		}
		base := uint64(*at)
		*at += size
		o.Syms = append(o.Syms, obj.Sym{
			Name: sanitize(d.Name), Section: sec, Value: base, Size: uint64(size),
			Global: d.Linkage.Export, TLS: sec == obj.SecTbss,
		})
		return nil
	}

	// A thread-local's bytes are not data used in place: they are the image every
	// new thread's block is initialized from. They go to .tdata, and the symbol
	// records an offset within that block rather than an address.
	buf, sec := &o.Data, obj.SecData
	align := &o.DataAlign
	switch {
	case d.Linkage.Thread:
		buf, sec, align = &o.Tdata, obj.SecTdata, &o.TlsAlign
	case d.Linkage.Section == rodataSection && d.HoldsAddress():
		// Read-only, and yet it cannot be finished here: the address it holds is
		// only known once the image is loaded, and .rodata is mapped unwritable.
		// .data.rel.ro is the compromise -- writable while the loader relocates it,
		// read-only by PT_GNU_RELRO before the program runs.
		buf, sec, align = &o.Relro, obj.SecRelro, &o.RelroAlign
	case d.Linkage.Section == rodataSection:
		// Read-only data is a section of its own, because a section is where the
		// promise is kept: the loader maps it without write permission.
		buf, sec, align = &o.Rodata, obj.SecRodata, &o.RodataAlign
	}
	if d.Align > *align {
		*align = d.Align
	}
	if d.Align > 0 {
		for len(*buf)%d.Align != 0 {
			*buf = append(*buf, 0)
		}
	}
	base := uint64(len(*buf))
	for _, it := range d.Items {
		switch {
		case it.Zero > 0:
			*buf = append(*buf, make([]byte, it.Zero)...)
		case it.Str != "":
			*buf = append(*buf, []byte(it.Str)...)
		case it.Sym != "":
			// A pointer stored in data: emit an 8-byte slot and a .rela.data
			// ABS64 relocation carrying the (symbol + offset) addend.
			if sec == obj.SecTdata {
				return fmt.Errorf("arm64: thread-local %q cannot hold the address of %q: every thread's copy would need its own relocation", d.Name, it.Sym)
			}
			// The relocation's offset is relative to its own section, so it has to
			// be filed against the one the datum went to: a .data.rel.ro offset in
			// .rela.data patches whatever is at that offset in .data.
			//
			// .rodata is not a case here: a const datum holding an address is
			// exactly what went to .data.rel.ro above, so nothing reaching this
			// point is in .rodata.
			list := &o.DataRelocs
			if sec == obj.SecRelro {
				list = &o.RelroRelocs
			}
			*list = append(*list, obj.Reloc{
				Offset: uint64(len(*buf)), Sym: sanitize(it.Sym),
				Type: obj.R_AARCH64_ABS64, Addend: it.Off,
			})
			*buf = append(*buf, make([]byte, 8)...)
		case len(it.Flts) > 0:
			for _, v := range it.Flts {
				*buf = appendInt(*buf, floatBitsOf(it.Sub, v), it.Sub.Size())
			}
		default:
			for _, v := range it.Ints {
				*buf = appendInt(*buf, v, it.Sub.Size())
			}
		}
	}
	o.Syms = append(o.Syms, obj.Sym{
		Name: sanitize(d.Name), Section: sec, Value: base,
		Size: uint64(len(*buf)) - base, Global: d.Linkage.Export,
		TLS: sec == obj.SecTdata,
	})
	return nil
}

func appendInt(b []byte, v int64, size int) []byte {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(v))
	return append(b, buf[:size]...)
}

func floatBitsOf(sub ir.SubCls, v float64) int64 {
	if sub.Size() == 4 {
		return int64(math.Float32bits(float32(v)))
	}
	return int64(math.Float64bits(v))
}

// rootLoc is where a live GC reference sits at a safepoint: a register (kind 0,
// val = physical register number) or a frame slot (kind 1, val = byte offset
// from the frame pointer x29), plus its type descriptor.
type rootLoc struct {
	kind uint8
	val  int32
	typ  uint32
}

// safepoint is a call site's return address (func-relative) and the GC roots
// live across the call.
type safepoint struct {
	pc    uint64
	roots []rootLoc
}

// mc emits AArch64 machine code for one function.
type mc struct {
	f        *ir.Func
	alloc    *allocation
	gc       GCStrategy
	tlsModel TLSModel
	prog     *a64.Program
	relocs   []obj.Reloc

	frameLayout // where everything lives in the frame

	instrPC    map[*ir.Instr]uint64 // PC at the start of each instruction
	safepoints []safepoint

	blockDone bool
	vaSeq     int
	rows      []obj.LineRow // source positions, keyed by func-relative byte offset
	lastPos   ir.SrcPos
	inl       []inlSample // inline-context changes, keyed by func-relative offset
	lastInl   *ir.InlineSite
	err       error
}

// inlSample records that, from byte offset off onward, emitted code belongs to
// the inline context site (nil = ordinary, non-inlined code).
type inlSample struct {
	off  uint64
	site *ir.InlineSite
}

// machineCode is the emitted result of one function: the bytes plus the debug
// and GC metadata gathered during emission.
type machineCode struct {
	code       []byte
	relocs     []obj.Reloc
	rows       []obj.LineRow
	inl        []inlSample
	safepoints []safepoint
	blockSyms  []blockSym // local symbols for address-taken blocks (&&label in data)
	m          *mc        // retained so callers can query variable locations
}

// blockSym is a local symbol placed at a block's byte offset within its function.
type blockSym struct {
	name string
	off  int
}

func emitMachine(f *ir.Func, alloc *allocation, gc GCStrategy, tlsModel TLSModel) (*machineCode, error) {
	m := &mc{
		f: f, alloc: alloc, gc: gc, tlsModel: tlsModel,
		prog: a64.NewProgram(), instrPC: map[*ir.Instr]uint64{},
		frameLayout: computeFrame(f, alloc),
	}
	m.prologue()
	for _, b := range f.Blocks {
		m.prog.Label(b.Name)
		m.block(b)
	}
	if m.err != nil {
		return nil, m.err
	}
	code, err := m.prog.Bytes()
	if err != nil {
		return nil, err
	}
	var blockSyms []blockSym
	for _, b := range f.Blocks {
		if b.Sym == "" {
			continue
		}
		if off, ok := m.prog.LabelOffset(b.Name); ok {
			blockSyms = append(blockSyms, blockSym{name: sanitize(b.Sym), off: off})
		}
	}
	return &machineCode{code: code, relocs: m.relocs, rows: m.rows, inl: m.inl, safepoints: m.safepoints, blockSyms: blockSyms, m: m}, nil
}

// recordLoc appends a line-table row when the source position changes: the row
// is the line program's own record of which instruction belongs to which line.
func (m *mc) recordLoc(pos ir.SrcPos) {
	if !pos.Valid() || pos == m.lastPos {
		return
	}
	m.lastPos = pos
	m.rows = append(m.rows, obj.LineRow{
		TextOff: uint64(m.prog.Len() * 4),
		File:    int(pos.File), Line: int(pos.Line), Col: int(pos.Col),
	})
}

// recordInline notes a change of inline context, so the emitter can later carve
// the function's code into inlined-subroutine ranges.
func (m *mc) recordInline(site *ir.InlineSite) {
	if site == m.lastInl {
		return
	}
	m.lastInl = site
	m.inl = append(m.inl, inlSample{off: uint64(m.prog.Len() * 4), site: site})
}

// locOf is where a value lives. A symbol address has no location -- it must be
// materialized -- so asking for one is an error.
func (m *mc) locOf(ref ir.Ref) loc {
	l, ok := locOf(m.f, ref)
	if !ok {
		m.fail("arm64: cannot move operand %v", ref)
	}
	return l
}

func (m *mc) fail(format string, a ...any) {
	if m.err == nil {
		m.err = fmt.Errorf(format, a...)
	}
}

func (m *mc) emit(w uint32) { m.prog.Emit(w) }

// mreg maps a cg12 physical register to its raw 5-bit encoding.
func mreg(r Reg) a64.Reg {
	switch {
	case r >= V0:
		return a64.Reg(r - V0)
	case r == SP, r == ZR:
		return 31
	default:
		return a64.Reg(r)
	}
}

// scratch registers, mirroring reg.go: x16/x17 (+ x15) for GP, v30/v31 for FP.
var (
	mcIntScratch = [3]a64.Reg{16, 17, 15} // slot 2 (x15) is used by 3-operand indexed stores
	mcFPScratch  = [2]a64.Reg{30, 31}
)

const (
	mcGP0 = a64.Reg(16)
	mcGP1 = a64.Reg(17)
	mcGP2 = a64.Reg(15)
	mcFP0 = a64.Reg(30)
	mcX29 = a64.Reg(29)
	mcX30 = a64.Reg(30)
	mcSP  = a64.Reg(31)
)

// --- prologue / epilogue ---------------------------------------------------

func (m *mc) prologue() {
	// A strategy may emit a stack-growth guard before the frame is set up; its
	// slow path branches back to this label to re-check after the stack grows.
	if pe, ok := m.gc.(PrologueEmitter); ok {
		m.prog.Label("__cg12_prologue")
		pe.EmitPrologue(&PrologueContext{mc: m, retry: "__cg12_prologue"})
	}
	m.allocFrame()
	for i, r := range m.calleeSaved {
		m.spillStore(mreg(r), r.IsFloat(), 16+i*8, 8)
	}
	if m.variadic {
		for i := 0; i < 8; i++ {
			m.emit(a64.StrImm(true, a64.Reg(i), mcX29, uint32(m.gpSaveOff+i*8)))
		}
		for i := 0; i < 8; i++ {
			m.emit(a64.StrFP(true, a64.Reg(i), mcX29, uint32(m.fpSaveOff+i*16)))
		}
	}
}

func (m *mc) allocFrame() {
	if m.frame <= 504 {
		m.emit(a64.Stp(true, mcX29, mcX30, mcSP, -m.frame, a64.PreIndex))
	} else {
		m.adjustSP(true, m.frame)
		m.emit(a64.Stp(true, mcX29, mcX30, mcSP, 0, a64.SignedOffset))
	}
	m.emit(a64.AddImm(true, mcX29, mcSP, 0)) // mov x29, sp
}

func (m *mc) frameTeardown() {
	(&sel{f: m.f, b: &mcAsm{prog: m.prog, m: m}, spillBase: m.spillBase}).
		frameTeardown(m.frame, m.hasDynAlloc, m.calleeSaved)
}

func (m *mc) epilogue() {
	(&sel{f: m.f, b: &mcAsm{prog: m.prog, m: m}, spillBase: m.spillBase}).
		epilogue(m.frame, m.hasDynAlloc, m.calleeSaved)
}

// frameAddr computes x29 + off into d. The ADD immediate is only 12 bits (0..4095),
// so a larger frame offset is split into a shifted-high and a low add — otherwise
// the immediate silently wraps and the address collides with another frame slot.
func (m *mc) frameAddr(d a64.Reg, off int) {
	hi, lo := off>>12, off&0xfff
	if hi > 0 {
		m.emit(a64.AddImmLSL12(true, d, mcX29, uint32(hi)))
		if lo > 0 {
			m.emit(a64.AddImm(true, d, d, uint32(lo)))
		}
		return
	}
	m.emit(a64.AddImm(true, d, mcX29, uint32(lo)))
}

// adjustSP subtracts (sub=true) or adds n to sp. It uses only the immediate
// forms (a shifted #hi12<<12 plus a #lo12), because the register form of add/sub
// cannot target sp — register 31 there denotes the zero register, so
// `sub sp, sp, xN` would silently write to xzr. Splitting the immediate reaches
// any frame up to ~16 MiB.
func (m *mc) adjustSP(sub bool, n int) {
	if n == 0 {
		return
	}
	hi, lo := n>>12, n&0xfff
	if hi > 4095 {
		m.fail("arm64: frame size %d too large for the machine-code emitter", n)
		return
	}
	imm := func(v uint32, lsl12 bool) {
		switch {
		case sub && lsl12:
			m.emit(a64.SubImmLSL12(true, mcSP, mcSP, v))
		case sub:
			m.emit(a64.SubImm(true, mcSP, mcSP, v))
		case lsl12:
			m.emit(a64.AddImmLSL12(true, mcSP, mcSP, v))
		default:
			m.emit(a64.AddImm(true, mcSP, mcSP, v))
		}
	}
	if hi > 0 {
		imm(uint32(hi), true)
	}
	if lo > 0 {
		imm(uint32(lo), false)
	}
}

// spillImmFits reports whether a byte offset encodes directly in the scaled
// 12-bit unsigned-immediate field of a load/store of the given access width.
func spillImmFits(off, size int) bool {
	return off >= 0 && off%size == 0 && off/size <= 4095
}

// ldFrom / stTo emit a load/store of r from/to [base, #off], selecting the quad,
// FP, or GP form. off must fit the scaled immediate.
func (m *mc) ldFrom(r a64.Reg, float bool, base a64.Reg, off, size int) {
	switch {
	case size == 16:
		m.emit(a64.LdrQ(r, base, uint32(off))) // 128-bit quad
	case float:
		m.emit(a64.LdrFP(size == 8, r, base, uint32(off)))
	default:
		m.emit(a64.LdrImm(size == 8, r, base, uint32(off)))
	}
}

func (m *mc) stTo(r a64.Reg, float bool, base a64.Reg, off, size int) {
	switch {
	case size == 16:
		m.emit(a64.StrQ(r, base, uint32(off)))
	case float:
		m.emit(a64.StrFP(size == 8, r, base, uint32(off)))
	default:
		m.emit(a64.StrImm(size == 8, r, base, uint32(off)))
	}
}

// spillLoad / spillStore move a value between a register and a frame slot at
// [x29, #off]. A frame large enough to push a slot past the scaled 12-bit
// immediate range is handled by materializing the address in a scratch: a GP
// load reuses its own destination (an FP load borrows x16, which is never live
// across an FP operand load), and a store uses x17 (never a spill value and dead
// at every store point, so it cannot alias r).
func (m *mc) spillLoad(r a64.Reg, float bool, off, size int) {
	if spillImmFits(off, size) {
		m.ldFrom(r, float, mcX29, off, size)
		return
	}
	addr := r
	if float {
		addr = mcGP0
	}
	m.frameAddr(addr, off)
	m.ldFrom(r, float, addr, 0, size)
}

func (m *mc) spillStore(r a64.Reg, float bool, off, size int) {
	if spillImmFits(off, size) {
		m.stTo(r, float, mcX29, off, size)
		return
	}
	m.frameAddr(mcGP1, off)
	m.stTo(r, float, mcGP1, 0, size)
}

// emitReg emits `mov dst, src` for a register-variable read or write. The
// stack pointer (register 31) cannot use the orr-based mov encoding, so an
// add-#0 is used when it is involved; floats use fmov.
func (m *mc) emitReg(cls ir.Cls, dst, src a64.Reg) {
	switch {
	case cls == ir.ClsQ:
		m.emit(a64.MovVec16b(dst, src)) // full 128-bit SIMD copy
	case cls.IsFloat():
		m.emit(a64.FmovReg(cls.Size() == 8, dst, src))
	case dst == 31 || src == 31:
		m.emit(a64.AddImm(true, dst, src, 0)) // works with sp, unlike mov/orr
	default:
		m.emit(a64.MovReg(cls.Size() == 8, dst, src))
	}
}

func (m *mc) movImm(r a64.Reg, val int64, w64 bool) {
	u := uint64(val)
	chunks := 4
	if !w64 {
		u &= 0xffffffff
		chunks = 2
	}
	// Choose a MOVZ or MOVN base by whichever leaves fewer 16-bit chunks to fill:
	// MOVZ starts from zero (skip 0x0000 chunks), MOVN starts from all-ones (skip
	// 0xffff chunks). A value dominated by set bits (e.g. -1) then costs one insn.
	ones, zeros := 0, 0
	for i := 0; i < chunks; i++ {
		switch (u >> (16 * i)) & 0xffff {
		case 0xffff:
			ones++
		case 0:
			zeros++
		}
	}
	if ones > zeros {
		first := true
		for i := 0; i < chunks; i++ {
			c := uint16((u >> (16 * i)) & 0xffff)
			if c == 0xffff {
				continue
			}
			if first {
				m.emit(a64.Movn(w64, r, ^c, uint32(16*i)))
				first = false
			} else {
				m.emit(a64.Movk(w64, r, c, uint32(16*i)))
			}
		}
		if first { // every chunk was 0xffff: the value is all ones
			m.emit(a64.Movn(w64, r, 0, 0))
		}
		return
	}
	first := true
	for i := 0; i < chunks; i++ {
		c := uint16((u >> (16 * i)) & 0xffff)
		if c == 0 {
			continue
		}
		if first {
			m.emit(a64.Movz(w64, r, c, uint32(16*i)))
			first = false
		} else {
			m.emit(a64.Movk(w64, r, c, uint32(16*i)))
		}
	}
	if first { // the value is zero
		m.emit(a64.Movz(w64, r, 0, 0))
	}
}

// emitTLSIndexAddr loads the address of a thread-local's descriptor into r: the
// two words naming which module owns the variable and its offset within that
// module. Handing it to __tls_get_addr yields the address. This is plain adrp/add
// and needs only the register it writes -- the call is a separate instruction, so
// the allocator knows to spill across it.
func (m *mc) emitTLSIndexAddr(r a64.Reg, c ir.Const) {
	sym := sanitize(c.Sym)
	m.reloc(sym, obj.R_AARCH64_TLSGD_ADR_PAGE21)
	m.emit(a64.Adrp(r, 0))
	m.reloc(sym, obj.R_AARCH64_TLSGD_ADD_LO12_NC)
	m.emit(a64.AddImm(true, r, r, 0))
}

// emitTLSOffset loads a thread-local's offset from the thread pointer into r.
// Initial-exec reads it from a GOT slot the loader fills, so this is an ordinary
// PC-relative load and needs only the register it writes -- the thread pointer is
// a separate op, and the sum an ordinary add.
func (m *mc) emitTLSOffset(r a64.Reg, c ir.Const) {
	if m.tlsModel == TLSLocalExec {
		m.fail("arm64: local-exec computes a thread-local's offset in the instruction itself, not into a register")
		return
	}
	sym := sanitize(c.Sym)
	m.reloc(sym, obj.R_AARCH64_TLSIE_ADR_GOTTPREL_PAGE21)
	m.emit(a64.Adrp(r, 0))
	m.reloc(sym, obj.R_AARCH64_TLSIE_LD64_GOTTPREL_LO12_NC)
	m.emit(a64.LdrImm(true, r, r, 0))
}

// materializeSym loads a symbol address (plus offset) into r via adrp/add,
// recording the page and low-12 relocations.
func (m *mc) materializeSym(r a64.Reg, c ir.Const) {
	sym := sanitize(c.Sym)
	if c.Thread {
		// Only local-exec reaches here: the other models are lowered into ordinary
		// instructions before selection, so the register allocator can see them.
		// reg = thread_pointer + tprel(sym), which needs just this one register.
		m.emit(a64.MrsTPIDR(r))
		m.reloc(sym, obj.R_AARCH64_TLSLE_ADD_TPREL_HI12)
		m.emit(a64.AddImmLSL12(true, r, r, 0))
		m.reloc(sym, obj.R_AARCH64_TLSLE_ADD_TPREL_LO12_NC)
		m.emit(a64.AddImm(true, r, r, 0))
	} else {
		m.reloc(sym, obj.R_AARCH64_ADR_PREL_PG_HI21)
		m.emit(a64.Adrp(r, 0))
		m.reloc(sym, obj.R_AARCH64_ADD_ABS_LO12_NC)
		m.emit(a64.AddImm(true, r, r, 0))
	}
	if c.Int != 0 {
		if c.Int < 0 || c.Int > 4095 {
			m.fail("arm64: symbol offset %d out of range for the machine-code emitter", c.Int)
			return
		}
		m.emit(a64.AddImm(true, r, r, uint32(c.Int)))
	}
}

// reloc records a relocation at the current instruction position.
func (m *mc) reloc(sym string, typ uint32) {
	m.relocs = append(m.relocs, obj.Reloc{Offset: uint64(m.prog.Len() * 4), Sym: sym, Type: typ})
}

// --- operands --------------------------------------------------------------

// src returns a raw register holding ref's value, loading a spilled temporary or
// materializing a constant into a class-appropriate scratch (slot 0 or 1).
func (m *mc) src(ref ir.Ref, slot, size int) a64.Reg {
	if m.f.ClassOf(ref).IsFloat() {
		fs := mcFPScratch[slot]
		switch ref.Kind {
		case ir.RefTemp:
			t := m.f.Temps[ref.ID]
			if t.Reg != ir.NoReg {
				return mreg(Reg(t.Reg))
			}
			m.spillLoad(fs, true, m.spillBase+t.Slot, size)
			return fs
		case ir.RefConst:
			if bits, ok := floatConstBits(m.f.Consts[ref.ID]); ok {
				m.movImm(mcGP0, bits, size == 8)
				m.emit(a64.FmovFromGP(size == 8, fs, mcGP0))
				return fs
			}
		}
		m.fail("arm64: cannot materialize float operand %v", ref)
		return fs
	}
	scr := mcIntScratch[slot]
	switch ref.Kind {
	case ir.RefTemp:
		t := m.f.Temps[ref.ID]
		if t.Reg != ir.NoReg {
			return mreg(Reg(t.Reg))
		}
		m.spillLoad(scr, false, m.spillBase+t.Slot, size)
		return scr
	case ir.RefConst:
		c := m.f.Consts[ref.ID]
		switch c.Kind {
		case ir.ConstInt:
			m.movImm(scr, c.Int, size == 8)
			return scr
		case ir.ConstSym:
			m.materializeSym(scr, c)
			return scr
		}
	}
	m.fail("arm64: cannot materialize operand %v", ref)
	return scr
}

// dst returns the destination register for a result plus a finalizer that stores
// it back when the result is spilled.
func (m *mc) dst(ref ir.Ref, size int) (a64.Reg, func()) {
	t := m.f.Temps[ref.ID]
	if t.Reg != ir.NoReg {
		return mreg(Reg(t.Reg)), func() {}
	}
	float := t.Cls.IsFloat()
	scr := mcGP0
	if float {
		scr = mcFP0
	}
	off := m.spillBase + t.Slot
	return scr, func() { m.spillStore(scr, float, off, size) }
}

// --- parallel moves --------------------------------------------------------

func (m *mc) parallelMove(pairs []movePairLoc) {
	(&sel{f: m.f, b: &mcAsm{prog: m.prog, m: m}, spillBase: m.spillBase}).parallelMove(pairs)
}

func (m *mc) emitMoveLoc(dst, src loc) {
	size := dst.size
	w64 := size == 8
	switch {
	case !dst.mem:
		dr := mreg(dst.reg)
		switch {
		case src.imm:
			if dst.reg.IsFloat() {
				m.movImm(mcGP0, src.val, w64)
				m.emit(a64.FmovFromGP(w64, dr, mcGP0))
			} else {
				m.movImm(dr, src.val, w64)
			}
		case src.mem:
			m.spillLoad(dr, dst.reg.IsFloat(), m.spillBase+src.slot, size)
		case size == 16:
			m.emit(a64.MovVec16b(dr, mreg(src.reg))) // full 128-bit SIMD copy
		case dst.reg.IsFloat():
			m.emit(a64.FmovReg(w64, dr, mreg(src.reg)))
		default:
			m.emit(a64.MovReg(w64, dr, mreg(src.reg)))
		}
	default: // dst is a spill slot
		switch {
		case src.imm:
			m.movImm(mcGP0, src.val, w64)
			m.spillStore(mcGP0, false, m.spillBase+dst.slot, size)
		case src.mem:
			m.spillLoad(mcGP0, false, m.spillBase+src.slot, size)
			m.spillStore(mcGP0, false, m.spillBase+dst.slot, size)
		default:
			m.spillStore(mreg(src.reg), src.reg.IsFloat(), m.spillBase+dst.slot, size)
		}
	}
}

// --- block / call sequences ------------------------------------------------

func (m *mc) block(b *ir.Block) {
	i := 0
	if b == m.f.Start {
		var pairs []movePairLoc
		var stackParams []*ir.Instr
		for i < len(b.Instrs) && b.Instrs[i].Op == ir.OPar {
			in := &b.Instrs[i]
			if len(in.Args) == 0 {
				stackParams = append(stackParams, in)
			} else {
				pairs = append(pairs, movePairLoc{dst: m.locOf(in.To), src: m.locOf(in.Args[0])})
			}
			i++
		}
		m.parallelMove(pairs)
		for _, in := range stackParams {
			m.stackParam(in)
		}
	}
	m.blockDone = false
	for i < len(b.Instrs) {
		in := &b.Instrs[i]
		m.instrPC[in] = uint64(m.prog.Len() * 4)
		m.recordLoc(in.Pos)
		m.recordInline(in.Inl)
		if in.Op == ir.OArg {
			i = m.callSequence(b, i)
			continue
		}
		m.instr(in)
		i++
	}
	if !m.blockDone {
		m.term(b)
	}
}

func (m *mc) stackParam(in *ir.Instr) {
	off := m.frame + int(in.Aux)
	t := m.f.Temps[in.To.ID]
	if t.Agg != nil {
		if t.Reg != ir.NoReg {
			m.frameAddr(mreg(Reg(t.Reg)), off)
			return
		}
		m.frameAddr(mcGP0, off)
		m.spillStore(mcGP0, false, m.spillBase+t.Slot, 8)
		return
	}
	sz := in.Cls.Size()
	float := in.Cls.IsFloat()
	if t.Reg != ir.NoReg {
		m.spillFromFrame(mreg(Reg(t.Reg)), float, off, sz)
		return
	}
	scr := mcGP0
	if float {
		scr = mcFP0
	}
	m.spillFromFrame(scr, float, off, sz)
	m.spillStore(scr, float, m.spillBase+t.Slot, sz)
}

// spillFromFrame loads from an arbitrary [x29, #off] (the incoming-argument
// area), unlike spillLoad which reads the local spill area.
func (m *mc) spillFromFrame(r a64.Reg, float bool, off, size int) {
	if spillImmFits(off, size) {
		m.ldFrom(r, float, mcX29, off, size)
		return
	}
	addr := r
	if float {
		addr = mcGP0
	}
	m.frameAddr(addr, off)
	m.ldFrom(r, float, addr, 0, size)
}

func (m *mc) callSequence(b *ir.Block, i int) int {
	var regPairs []movePairLoc
	var stackArgs, symArgs []*ir.Instr
	for i < len(b.Instrs) && b.Instrs[i].Op == ir.OArg {
		a := &b.Instrs[i]
		switch {
		case a.To.IsNone():
			stackArgs = append(stackArgs, a)
		case isConstSym(m.f, a.Args[0]):
			symArgs = append(symArgs, a)
		default:
			regPairs = append(regPairs, movePairLoc{dst: m.locOf(a.To), src: m.locOf(a.Args[0])})
		}
		i++
	}
	call := &b.Instrs[i]
	m.recordLoc(call.Pos) // the OArg run carries no position; the call does
	m.recordInline(call.Inl)

	if call.Tail {
		for _, a := range stackArgs {
			r := m.src(a.Args[0], 0, a.Cls.Size())
			m.emit(a64.StrImm(a.Cls.Size() == 8, r, mcX29, uint32(m.frame+int(a.Aux))))
		}
		m.parallelMove(regPairs)
		for _, a := range symArgs {
			m.materializeSym(mreg(Reg(m.f.Temps[a.To.ID].Reg)), m.f.Consts[a.Args[0].ID])
		}
		m.tailBranch(call)
		m.blockDone = true
		return i + 1
	}

	stackBytes := int(call.Aux)
	if stackBytes > 0 {
		m.adjustSP(true, stackBytes)
	}
	for _, a := range stackArgs {
		sz := a.Cls.Size()
		r := m.src(a.Args[0], 0, sz)
		if a.Cls.IsFloat() {
			m.emit(a64.StrFP(sz == 8, r, mcSP, uint32(a.Aux)))
		} else {
			m.emit(a64.StrImm(sz == 8, r, mcSP, uint32(a.Aux)))
		}
	}
	m.parallelMove(regPairs)
	for _, a := range symArgs {
		m.materializeSym(mreg(Reg(m.f.Temps[a.To.ID].Reg)), m.f.Consts[a.Args[0].ID])
	}
	m.emitCall(call)
	if stackBytes > 0 {
		m.adjustSP(false, stackBytes)
	}
	return i + 1
}

func (m *mc) emitCall(in *ir.Instr) {
	(&sel{f: m.f, b: &mcAsm{prog: m.prog, m: m}, spillBase: m.spillBase}).call(in)
	m.recordSafepoint(in) // the branch just emitted; the return address is next
}

// recordSafepoint records the GC roots live at the safepoint in, at the current
// PC, reading each root's location from its register/spill-slot binding. For a
// call this is invoked just after the branch (so the PC is the return address);
// for an explicit OSafepoint it is invoked at the marker (which emits no code).
func (m *mc) recordSafepoint(in *ir.Instr) {
	roots := m.alloc.safeRoots[in]
	if len(roots) == 0 {
		return
	}
	locs := make([]rootLoc, 0, len(roots))
	for _, id := range roots {
		t := m.f.Temps[id]
		if t.Reg != ir.NoReg {
			locs = append(locs, rootLoc{kind: rootReg, val: int32(mreg(Reg(t.Reg))), typ: t.GCType})
		} else {
			locs = append(locs, rootLoc{kind: rootFrame, val: int32(m.spillBase + t.Slot), typ: t.GCType})
		}
	}
	m.safepoints = append(m.safepoints, safepoint{pc: uint64(m.prog.Len() * 4), roots: locs})
}

const (
	rootReg   uint8 = 0 // val is a physical register number
	rootFrame uint8 = 1 // val is a byte offset from the frame pointer (x29)
	rootSP    uint8 = 2 // val is a byte offset from the stack pointer at the safepoint
)

// tempInterval returns a temporary's live range, or nil if it has none.
func (m *mc) tempInterval(id int) *interval {
	for _, iv := range m.alloc.intervals {
		if iv.temp == id {
			return iv
		}
	}
	return nil
}

// pcRange maps a live interval (in the allocator's numbering) to the [lo, hi) PC
// range spanned by its instructions. It takes the min/max emitted PC so it is
// robust to block-emission order.
func (m *mc) pcRange(start, end int) (lo, hi uint64, ok bool) {
	pi := m.alloc.posInstr
	lo = ^uint64(0)
	for q := start; q <= end && q < len(pi); q++ {
		if q < 0 {
			continue
		}
		if in := pi[q]; in != nil {
			if pc, o := m.instrPC[in]; o {
				if pc < lo {
					lo = pc
				}
				if pc+4 > hi {
					hi = pc + 4
				}
				ok = true
			}
		}
	}
	return lo, hi, ok
}

// varLoc computes a temporary's DWARF location (register or frame slot) over its
// live PC range, or nil when it has no determinable range.
func (m *mc) varLoc(tempID int) *obj.VarLoc {
	iv := m.tempInterval(tempID)
	if iv == nil {
		return nil
	}
	lo, hi, ok := m.pcRange(iv.start, iv.end)
	if !ok {
		return nil
	}
	t := m.f.Temps[tempID]
	var expr []byte
	if t.Reg != ir.NoReg {
		expr = obj.LocReg(dwarfRegNum(Reg(t.Reg)))
	} else {
		expr = obj.LocFrameBase(int64(m.spillBase + t.Slot))
	}
	return &obj.VarLoc{Lo: lo, Hi: hi, Expr: expr}
}

// dwarfRegNum maps a physical register to its DWARF register number: X0..X30 are
// 0..30, V0..V31 are 64..95.
func dwarfRegNum(r Reg) uint32 {
	if r >= V0 {
		return 64 + uint32(r-V0)
	}
	return uint32(mreg(r))
}

func (m *mc) tailBranch(in *ir.Instr) {
	callee := in.Args[0]
	switch callee.Kind {
	case ir.RefConst:
		c := m.f.Consts[callee.ID]
		if c.Kind != ir.ConstSym {
			m.fail("arm64: tail call target must be a symbol or register")
			return
		}
		m.frameTeardown()
		m.reloc(sanitize(c.Sym), obj.R_AARCH64_JUMP26)
		m.emit(a64.B(0))
	case ir.RefTemp:
		r := m.src(callee, 0, 8)
		m.emit(a64.MovReg(true, mreg(scratch1), r)) // survive the teardown
		m.frameTeardown()
		m.emit(a64.Br(mreg(scratch1)))
	default:
		m.fail("arm64: invalid tail call target")
	}
}

// --- instructions ----------------------------------------------------------

func (m *mc) instr(in *ir.Instr) {
	// Integer data-processing instructions are selected once, through the shared
	// builder (here backed by the machine-code encoder).
	if (&sel{f: m.f, b: &mcAsm{prog: m.prog, m: m}, spillBase: m.spillBase}).selectData(in) {
		return
	}
	switch in.Op {
	case ir.ONop:
		return
	case ir.OCopy, ir.OPar, ir.OArg:
		m.copy(in)
	// The data-processing, compare, conversion, extend, and load/store
	// instructions are selected by the shared builder (selectData) before this
	// switch is reached.
	case ir.OCall:
		if in.Tail {
			m.tailBranch(in)
			m.blockDone = true
		} else {
			m.emitCall(in)
		}
	case ir.OAsm:
		m.emitAsm(in)
	case ir.OGetReg:
		phys := mreg(Reg(in.Args[0].ID))
		d, done := m.dst(in.To, in.Cls.Size())
		m.emitReg(in.Cls, d, phys)
		done()
	case ir.OSetReg:
		phys := mreg(Reg(in.Args[1].ID))
		v := m.src(in.Args[0], 0, in.Cls.Size())
		m.emitReg(in.Cls, phys, v)
	case ir.OSafepoint:
		// Let the GC strategy emit code here (e.g. a poll); the stack map is then
		// recorded at the resulting PC — the point a collection would resume at.
		if m.gc != nil {
			m.gc.EmitSafepoint(&GCContext{mc: m, roots: m.gcRoots(in)})
		}
		m.recordSafepoint(in)
	case ir.OVaStart:
		m.vaStart(in)
	case ir.OVaArg:
		m.vaArg(in)
	case ir.OAlloc4, ir.OAlloc8, ir.OAlloc16:
		d, done := m.dst(in.To, 8)
		m.frameAddr(d, m.allocOff[in])
		done()
	default:
		if in.Op.IsLoad() {
			m.load(in)
		} else if in.Op.IsStore() {
			m.store(in)
		} else {
			m.fail("arm64: op %s not supported by the machine-code emitter", in.Op)
		}
	}
}

func (m *mc) binInt(in *ir.Instr, enc func(w64 bool, rd, rn, rm a64.Reg) uint32) {
	sz := in.Cls.Size()
	s1 := m.src(in.Args[0], 0, sz)
	s2 := m.src(in.Args[1], 1, sz)
	d, done := m.dst(in.To, sz)
	m.emit(enc(sz == 8, d, s1, s2))
	done()
}

// shift emits an integer shift, using the immediate form when the amount is a
// constant in range and the variable form otherwise.
func (m *mc) shift(in *ir.Instr, vEnc func(w64 bool, rd, rn, rm a64.Reg) uint32, iEnc func(w64 bool, rd, rn a64.Reg, sh uint32) uint32) {
	sz := in.Cls.Size()
	if v, ok := intConst(m.f, in.Args[1]); ok {
		if sh, ok := shiftImm(v, sz); ok {
			s1 := m.src(in.Args[0], 0, sz)
			d, done := m.dst(in.To, sz)
			m.emit(iEnc(sz == 8, d, s1, sh))
			done()
			return
		}
	}
	m.binInt(in, vEnc)
}

// addSub emits an integer add or sub, folding a constant operand into a 12-bit
// (optionally << 12) immediate, flipping add<->sub for a negative constant.
func (m *mc) addSub(in *ir.Instr, sub bool) {
	sz := in.Cls.Size()
	aRef, bRef := in.Args[0], in.Args[1]
	if !sub { // add is commutative: fold whichever operand is the constant
		if _, ok := intConst(m.f, bRef); !ok {
			if _, ok := intConst(m.f, aRef); ok {
				aRef, bRef = bRef, aRef
			}
		}
	}
	if v, ok := intConst(m.f, bRef); ok {
		if imm, lsl12, flip, ok := addSubImm(v); ok {
			s1 := m.src(aRef, 0, sz)
			d, done := m.dst(in.To, sz)
			w64 := sz == 8
			switch useSub := sub != flip; {
			case useSub && lsl12:
				m.emit(a64.SubImmLSL12(w64, d, s1, imm))
			case useSub:
				m.emit(a64.SubImm(w64, d, s1, imm))
			case lsl12:
				m.emit(a64.AddImmLSL12(w64, d, s1, imm))
			default:
				m.emit(a64.AddImm(w64, d, s1, imm))
			}
			done()
			return
		}
	}
	if sub {
		m.binInt(in, a64.SubReg)
	} else {
		m.binInt(in, a64.AddReg)
	}
}

// logical emits a bitwise and/orr/eor, folding a constant operand into a
// logical (bitmask) immediate when it encodes as one.
func (m *mc) logical(in *ir.Instr, rEnc func(w64 bool, rd, rn, rm a64.Reg) uint32, iEnc func(w64 bool, rd, rn a64.Reg, n, immr, imms uint32) uint32) {
	sz := in.Cls.Size()
	aRef, bRef := in.Args[0], in.Args[1]
	if _, ok := intConst(m.f, bRef); !ok { // commutative: put the constant second
		if _, ok := intConst(m.f, aRef); ok {
			aRef, bRef = bRef, aRef
		}
	}
	if v, ok := intConst(m.f, bRef); ok {
		val := uint64(v)
		if sz == 4 {
			val &= 0xffffffff
		}
		if n, immr, imms, ok := a64.EncodeBitmask(val, sz); ok {
			s1 := m.src(aRef, 0, sz)
			d, done := m.dst(in.To, sz)
			m.emit(iEnc(sz == 8, d, s1, n, immr, imms))
			done()
			return
		}
	}
	m.binInt(in, rEnc)
}

func (m *mc) rem(in *ir.Instr, div func(w64 bool, rd, rn, rm a64.Reg) uint32) {
	sz := in.Cls.Size()
	w64 := sz == 8
	s1 := m.src(in.Args[0], 0, sz)
	s2 := m.src(in.Args[1], 1, sz)
	m.emit(div(w64, mcGP2, s1, s2))         // q = n / d
	d, done := m.dst(in.To, sz)             // r = n - q*d; Msub reads q before it
	m.emit(a64.Msub(w64, d, mcGP2, s2, s1)) // writes d, so d==q (spill) is safe
	done()
}

func (m *mc) copy(in *ir.Instr) {
	sz := in.Cls.Size()
	s := m.src(in.Args[0], 0, sz)
	d, done := m.dst(in.To, sz)
	if d != s {
		switch {
		case in.Cls == ir.ClsQ:
			m.emit(a64.MovVec16b(d, s)) // full 128-bit SIMD copy
		case in.Cls.IsFloat():
			m.emit(a64.FmovReg(sz == 8, d, s))
		default:
			m.emit(a64.MovReg(sz == 8, d, s))
		}
	}
	done()
}

func (m *mc) cmp(in *ir.Instr) {
	argCls := m.f.ClassOf(in.Args[0])
	sz := argCls.Size()
	var cond a64.Cond
	var ok bool
	if argCls.IsFloat() {
		s1 := m.src(in.Args[0], 0, sz)
		s2 := m.src(in.Args[1], 1, sz)
		m.emit(a64.Fcmp(sz == 8, s1, s2))
		cond, ok = fpCondOf(in.Cmp)
	} else {
		s1 := m.src(in.Args[0], 0, sz)
		folded := false
		if v, vok := intConst(m.f, in.Args[1]); vok {
			if imm, lsl12, flip, iok := addSubImm(v); iok && !lsl12 && !flip {
				m.emit(a64.CmpImm(sz == 8, s1, imm))
				folded = true
			}
		}
		if !folded {
			s2 := m.src(in.Args[1], 1, sz)
			m.emit(a64.CmpReg(sz == 8, s1, s2))
		}
		cond, ok = intCondOf(in.Cmp)
	}
	if !ok {
		m.fail("arm64: unsupported comparison predicate %v", in.Cmp)
		return
	}
	d, done := m.dst(in.To, in.Cls.Size())
	m.emit(a64.Cset(false, d, cond))
	done()
}

func (m *mc) extend(in *ir.Instr) {
	sz := in.Cls.Size()
	s := m.src(in.Args[0], 1, m.f.ClassOf(in.Args[0]).Size())
	d, done := m.dst(in.To, sz)
	switch in.Op {
	case ir.OExtsb:
		m.emit(a64.Sxtb(sz == 8, d, s))
	case ir.OExtsh:
		m.emit(a64.Sxth(sz == 8, d, s))
	case ir.OExtsw:
		m.emit(a64.Sxtw(d, s))
	case ir.OExtub:
		m.emit(a64.Uxtb(d, s))
	case ir.OExtuh:
		m.emit(a64.Uxth(d, s))
	case ir.OExtuw:
		m.emit(a64.MovReg(false, d, s)) // mov Wd, Wn zero-extends into Xd
	}
	done()
}

func (m *mc) load(in *ir.Instr) {
	sz := loadSize(in.Op, in.Cls)
	if len(in.Args) == 2 { // indexed: [base, index] with extend/scale in Aux
		base := m.src(in.Args[0], 1, 8)
		option, s := decodeAmode(in.Aux)
		index := m.src(in.Args[1], 0, indexSize(option))
		d, done := m.dst(in.To, sz)
		switch in.Op {
		case ir.OLoadl:
			m.emit(a64.LdrReg(true, d, base, index, option, s))
		case ir.OLoaduw:
			m.emit(a64.LdrReg(false, d, base, index, option, s))
		case ir.OLoadsw:
			m.emit(a64.LdrswReg(d, base, index, option, s))
		case ir.OLoadub:
			m.emit(a64.LdrbReg(d, base, index, option, s))
		case ir.OLoadsb:
			m.emit(a64.LdrsbReg(sz == 8, d, base, index, option, s))
		case ir.OLoaduh:
			m.emit(a64.LdrhReg(d, base, index, option, s))
		case ir.OLoadsh:
			m.emit(a64.LdrshReg(sz == 8, d, base, index, option, s))
		default:
			m.fail("arm64: unsupported indexed load %s", in.Op)
		}
		done()
		return
	}
	addr := m.src(in.Args[0], 1, 8)
	d, done := m.dst(in.To, sz)
	switch in.Op {
	case ir.OLoadl:
		m.emit(a64.LdrImm(true, d, addr, 0))
	case ir.OLoaduw:
		m.emit(a64.LdrImm(false, d, addr, 0))
	case ir.OLoadsw:
		m.emit(a64.LdrswImm(d, addr, 0))
	case ir.OLoadub:
		m.emit(a64.LdrbImm(d, addr, 0))
	case ir.OLoadsb:
		m.emit(a64.LdrsbImm(sz == 8, d, addr, 0))
	case ir.OLoaduh:
		m.emit(a64.LdrhImm(d, addr, 0))
	case ir.OLoadsh:
		m.emit(a64.LdrshImm(sz == 8, d, addr, 0))
	case ir.OLoads:
		m.emit(a64.LdrFP(false, d, addr, 0))
	case ir.OLoadd:
		m.emit(a64.LdrFP(true, d, addr, 0))
	case ir.OLoadq:
		m.emit(a64.LdrQ(d, addr, 0))
	default:
		m.fail("arm64: unsupported load %s", in.Op)
	}
	done()
}

func (m *mc) store(in *ir.Instr) {
	valSz := storeSize(in.Op)
	val := m.src(in.Args[0], 0, valSz)
	if len(in.Args) == 3 { // indexed: [value, base, index]
		base := m.src(in.Args[1], 1, 8)
		option, s := decodeAmode(in.Aux)
		index := m.src(in.Args[2], 2, indexSize(option))
		switch in.Op {
		case ir.OStorel:
			m.emit(a64.StrReg(true, val, base, index, option, s))
		case ir.OStorew:
			m.emit(a64.StrReg(false, val, base, index, option, s))
		case ir.OStoreb:
			m.emit(a64.StrbReg(val, base, index, option, s))
		case ir.OStoreh:
			m.emit(a64.StrhReg(val, base, index, option, s))
		default:
			m.fail("arm64: unsupported indexed store %s", in.Op)
		}
		return
	}
	addr := m.src(in.Args[1], 1, 8)
	switch in.Op {
	case ir.OStorel:
		m.emit(a64.StrImm(true, val, addr, 0))
	case ir.OStorew:
		m.emit(a64.StrImm(false, val, addr, 0))
	case ir.OStoreb:
		m.emit(a64.StrbImm(val, addr, 0))
	case ir.OStoreh:
		m.emit(a64.StrhImm(val, addr, 0))
	case ir.OStores:
		m.emit(a64.StrFP(false, val, addr, 0))
	case ir.OStored:
		m.emit(a64.StrFP(true, val, addr, 0))
	case ir.OStoreq:
		m.emit(a64.StrQ(val, addr, 0))
	default:
		m.fail("arm64: unsupported store %s", in.Op)
	}
}

// decodeAmode unpacks the extend option and scale bit an addressing fold stored
// in a memory op's Aux.
func decodeAmode(aux int64) (option, s uint32) {
	return uint32(aux>>1) & 7, uint32(aux & 1)
}

// indexSize is the spill-reload width of an index register: 32-bit for an SXTW
// index, 64-bit for a directly-used (LSL) index.
func indexSize(option uint32) int {
	if option == a64.ExtSXTW || option == a64.ExtUXTW {
		return 4
	}
	return 8
}

// loadSize is the width of the register a load writes. It is not simply the
// class size: a sub-word load zero-extends into a w register, and a sign-
// extending one widens to whatever its result class asks for.
func loadSize(op ir.Op, cls ir.Cls) int {
	switch op {
	case ir.OLoadub, ir.OLoaduh, ir.OLoaduw, ir.OLoads:
		return 4
	case ir.OLoadsb, ir.OLoadsh:
		return cls.Size()
	case ir.OLoadsw, ir.OLoadl, ir.OLoadd:
		return 8
	case ir.OLoadq:
		return 16
	}
	return 0
}

// storeSize is the width of the register a store reads.
func storeSize(op ir.Op) int {
	switch op {
	case ir.OStoreb, ir.OStoreh, ir.OStorew, ir.OStores:
		return 4
	case ir.OStorel, ir.OStored:
		return 8
	case ir.OStoreq:
		return 16
	}
	return 0
}

func (m *mc) term(b *ir.Block) {
	if (&sel{f: m.f, b: &mcAsm{prog: m.prog, m: m}, spillBase: m.spillBase}).term(b) {
		return
	}
	switch b.Jmp.Kind {
	case ir.JmpRet:
		m.epilogue()
	default:
		m.fail("arm64: block %q has no terminator", b.Name)
	}
}

func intCondOf(c ir.Cmp) (a64.Cond, bool) {
	switch c {
	case ir.CmpEq:
		return a64.EQ, true
	case ir.CmpNe:
		return a64.NE, true
	case ir.CmpSlt:
		return a64.LT, true
	case ir.CmpSle:
		return a64.LE, true
	case ir.CmpSgt:
		return a64.GT, true
	case ir.CmpSge:
		return a64.GE, true
	case ir.CmpUlt:
		return a64.CC, true
	case ir.CmpUle:
		return a64.LS, true
	case ir.CmpUgt:
		return a64.HI, true
	case ir.CmpUge:
		return a64.CS, true
	}
	return 0, false
}

func fpCondOf(c ir.Cmp) (a64.Cond, bool) {
	switch c {
	case ir.CmpFeq:
		return a64.EQ, true
	case ir.CmpFne:
		return a64.NE, true
	case ir.CmpFlt:
		return a64.MI, true
	case ir.CmpFle:
		return a64.LS, true
	case ir.CmpFgt:
		return a64.GT, true
	case ir.CmpFge:
		return a64.GE, true
	case ir.CmpFo:
		return a64.VC, true
	case ir.CmpFuo:
		return a64.VS, true
	}
	return 0, false
}

// --- variadics -------------------------------------------------------------

func (m *mc) vaPtr(ref ir.Ref) a64.Reg {
	t := m.f.Temps[ref.ID]
	if t.Reg != ir.NoReg {
		return mreg(Reg(t.Reg))
	}
	m.spillLoad(mreg(scratch2), false, m.spillBase+t.Slot, 8)
	return mreg(scratch2)
}

func (m *mc) vaStart(in *ir.Instr) {
	vp := m.vaPtr(in.Args[0])
	s := mreg(scratch1)
	m.frameAddr(s, m.frame+roundUp(m.namedStack, 8))
	m.emit(a64.StrImm(true, s, vp, 0)) // __stack
	m.frameAddr(s, m.gpSaveOff+8*8)
	m.emit(a64.StrImm(true, s, vp, 8)) // __gr_top
	m.frameAddr(s, m.fpSaveOff+8*16)
	m.emit(a64.StrImm(true, s, vp, 16)) // __vr_top
	m.movImm(mreg(scratch1), int64(-(8-m.namedGr)*8), false)
	m.emit(a64.StrImm(false, s, vp, 24)) // __gr_offs
	m.movImm(mreg(scratch1), int64(-(8-m.namedSr)*16), false)
	m.emit(a64.StrImm(false, s, vp, 28)) // __vr_offs
}

func (m *mc) vaArg(in *ir.Instr) {
	vp := m.vaPtr(in.Args[0])
	float := in.Cls.IsFloat()
	offsField, topField, stride := uint32(24), uint32(8), 8
	if float {
		offsField, topField, stride = 28, 16, 16
	}
	m.vaSeq++
	lStack := fmt.Sprintf("va_stack_%d", m.vaSeq)
	lDone := fmt.Sprintf("va_done_%d", m.vaSeq)

	offW := mreg(scratch1) // __gr_offs / __vr_offs
	addr := mcGP0          // computed address

	m.emit(a64.LdrImm(false, offW, vp, offsField))
	m.emit(a64.CmpReg(false, offW, a64.ZR))
	m.prog.Bcond(a64.GE, lStack)
	// register save area: addr = top + sext(offs); advance the offset
	m.emit(a64.LdrImm(true, addr, vp, topField))
	m.emit(a64.AddExtSxtw(addr, addr, offW))
	m.emit(a64.AddImm(false, offW, offW, uint32(stride)))
	m.emit(a64.StrImm(false, offW, vp, offsField))
	m.prog.B(lDone)
	// overflow stack: addr = __stack; advance __stack by one 8-byte slot
	m.prog.Label(lStack)
	m.emit(a64.LdrImm(true, addr, vp, 0))
	m.emit(a64.AddImm(true, mreg(scratch1), addr, 8))
	m.emit(a64.StrImm(true, mreg(scratch1), vp, 0))
	m.prog.Label(lDone)

	d, done := m.dst(in.To, in.Cls.Size())
	if float {
		m.emit(a64.LdrFP(in.Cls.Size() == 8, d, addr, 0))
	} else {
		m.emit(a64.LdrImm(in.Cls.Size() == 8, d, addr, 0))
	}
	done()
}

// allZero reports whether a data definition is nothing but zero fill, in which
// case it needs no bytes in the file at all -- the loader supplies the zeroes.
func allZero(d *ir.Data) bool {
	if len(d.Items) == 0 {
		return false
	}
	for _, it := range d.Items {
		if it.Zero == 0 {
			return false
		}
	}
	return true
}

// zeroSize is the total length of an all-zero definition.
func zeroSize(d *ir.Data) int {
	n := 0
	for _, it := range d.Items {
		n += it.Zero
	}
	return n
}

// emitAsm emits an inline-asm template as machine code. The operands are resolved
// to registers exactly as the ABI-independent template expects, the template is
// expanded, and the resulting assembly is run through the same assembler the
// text-input path uses -- so an inline-asm sequence becomes bytes without an
// external assembler. Only the mnemonics that assembler knows are available, and
// anything else is reported rather than silently dropped.
func (m *mc) emitAsm(in *ir.Instr) {
	asm := in.Asm
	vals := make([]asmVal, len(asm.Ops))
	scratchN := 0
	nextScratch := func() Reg {
		if scratchN >= len(intScratchRegs) {
			m.fail("arm64: inline asm needs more scratch registers than are available")
			return scratch0
		}
		r := intScratchRegs[scratchN]
		scratchN++
		return r
	}

	// Walk the operands in %N order, drawing register outputs from To/Defs and
	// every other operand's value from Args in order.
	outs := in.AsmRegOuts()
	oc, ac := 0, 0 // cursors into outs and in.Args
	var finals []func()
	resolveOut := func() (Reg, int) {
		oref := outs[oc]
		oc++
		t := m.f.Temps[oref.ID]
		w := m.f.ClassOf(oref).Size()
		if t.Reg != ir.NoReg {
			return Reg(t.Reg), w
		}
		r := nextScratch()
		slot := m.spillBase + t.Slot
		finals = append(finals, func() { m.spillStore(mreg(r), r.IsFloat(), slot, w) })
		return r, w
	}
	for i, kind := range asm.Ops {
		switch kind {
		case ir.AsmRegOut:
			r, w := resolveOut()
			vals[i] = asmVal{reg: r, width: w}
		case ir.AsmRegInOut:
			r, w := resolveOut()
			pre, _ := m.asmInputReg(in.Args[ac], nextScratch) // preload value
			ac++
			m.emit(a64.MovReg(w == 8, mreg(r), mreg(pre))) // preload the read-write register
			vals[i] = asmVal{reg: r, width: w}
		case ir.AsmImm:
			vals[i] = asmVal{lit: true, litS: fmt.Sprintf("#%d", m.f.Consts[in.Args[ac].ID].Int)}
			ac++
		case ir.AsmMem:
			r, _ := m.asmInputReg(in.Args[ac], nextScratch) // the operand's address
			vals[i] = asmVal{lit: true, litS: fmt.Sprintf("[%s]", r.xName())}
			ac++
		default: // AsmRegIn
			r, w := m.asmInputReg(in.Args[ac], nextScratch)
			vals[i] = asmVal{reg: r, width: w}
			ac++
		}
	}

	text, err := expandAsm(asm.Template, vals)
	if err != nil {
		m.fail("arm64: %v", err)
		return
	}
	code, err := a64.Assemble(text)
	if err != nil {
		m.fail("arm64: inline assembly: %v", err)
		return
	}
	for i := 0; i+4 <= len(code); i += 4 {
		m.emit(binary.LittleEndian.Uint32(code[i:]))
	}
	for _, f := range finals {
		f()
	}
}

// asmInputReg yields a register holding an inline-asm operand's value, loading a
// spilled temporary or materializing a constant into a scratch register first.
func (m *mc) asmInputReg(ref ir.Ref, nextScratch func() Reg) (Reg, int) {
	switch ref.Kind {
	case ir.RefTemp:
		t := m.f.Temps[ref.ID]
		w := m.f.ClassOf(ref).Size()
		if t.Reg != ir.NoReg {
			return Reg(t.Reg), w
		}
		r := nextScratch()
		m.spillLoad(mreg(r), r.IsFloat(), m.spillBase+t.Slot, w)
		return r, w
	case ir.RefConst:
		c := m.f.Consts[ref.ID]
		r := nextScratch()
		switch c.Kind {
		case ir.ConstInt:
			m.movImm(mreg(r), c.Int, true)
		case ir.ConstSym:
			m.materializeSym(mreg(r), c)
		default:
			m.fail("arm64: unsupported inline-asm constant operand")
		}
		return r, 8
	}
	m.fail("arm64: unsupported inline-asm operand %v", ref)
	return scratch0, 8
}

// rodataSection is the ir.Linkage.Section a front end marks read-only data with.
const rodataSection = ".rodata"
