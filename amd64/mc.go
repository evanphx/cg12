package amd64

import (
	"fmt"
	"math"
	"sort"

	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// Options controls object emission. GC supplies a pluggable garbage-collector
// strategy that emits safepoint (and optionally prologue) code late during
// emission; nil leaves safepoints as code-free stack-map markers.
type Options struct {
	GC GCStrategy
}

// CompileObject compiles a module straight to ELF x86-64 relocatable-object
// bytes with the machine-code emitter (no external assembler).
func CompileObject(m *ir.Module) ([]byte, error) {
	return CompileObjectWith(m, Options{})
}

// CompileObjectWith compiles a module to ELF bytes with the given options.
func CompileObjectWith(m *ir.Module, opts Options) ([]byte, error) {
	o, err := CompileToObjectWith(m, opts)
	if err != nil {
		return nil, err
	}
	return o.MarshalELF()
}

// CompileToObject compiles a module to an in-memory relocatable object.
func CompileToObject(m *ir.Module) (*obj.Object, error) {
	return CompileToObjectWith(m, Options{})
}

// CompileToObjectWith compiles a module to an in-memory relocatable object with
// the given options.
func CompileToObjectWith(m *ir.Module, opts Options) (*obj.Object, error) {
	o := &obj.Object{Machine: obj.EM_X86_64}
	var smFuncs []stackMapFunc
	var rows []obj.LineRow
	var dfuncs []obj.DwarfFunc
	anchor := ""
	for _, f := range m.Funcs {
		name := sanitize(f.Name)
		params := dwarfParams(f) // captured before lowering rewrites the params
		paramTemps := paramTempIDs(f)
		ir.LowerPointers(f, ir.ClsL)
		if err := lower(f); err != nil {
			return nil, fmt.Errorf("function %s: %w", f.Name, err)
		}
		alloc, err := regAlloc(f)
		if err != nil {
			return nil, fmt.Errorf("function %s: %w", f.Name, err)
		}
		mc, err := emitMachine(f, alloc, opts.GC)
		if err != nil {
			return nil, fmt.Errorf("function %s: %w", f.Name, err)
		}
		base := len(o.Text)
		if anchor == "" {
			anchor = name
		}
		o.Text = append(o.Text, mc.code...)
		for _, r := range mc.relocs {
			r.Offset += uint64(base)
			o.Relocs = append(o.Relocs, r)
		}
		o.Syms = append(o.Syms, obj.Sym{
			Name: name, Section: obj.SecText, Value: uint64(base),
			Size: uint64(len(mc.code)), Global: f.Linkage.Export, Func: true,
		})
		// Local symbols at address-taken blocks (&&label used in static data).
		for _, bs := range mc.blockSyms {
			o.Syms = append(o.Syms, obj.Sym{
				Name: bs.name, Section: obj.SecText, Value: uint64(base + bs.off),
			})
		}
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
			r.TextOff += uint64(base)
			rows = append(rows, r)
		}
		if len(mc.safepoints) > 0 {
			smFuncs = append(smFuncs, stackMapFunc{sym: name, points: mc.safepoints})
		}
	}
	for _, d := range m.Data {
		addData(o, d)
	}
	if len(m.Files) > 0 && anchor != "" {
		o.SetDWARF(m.Files, rows, dfuncs, uint64(len(o.Text)), anchor, "cg12", ".", m.Files[0], obj.R_X86_64_64, 0x56) // DW_OP_reg6 (rbp)
	}
	if len(smFuncs) > 0 {
		setStackMap(o, smFuncs)
	}
	return o, nil
}

// dwarfType maps an IR class to a DWARF base type (DW_ATE_signed = 5,
// DW_ATE_float = 4).
func dwarfType(cls ir.Cls) obj.DwarfType {
	switch cls {
	case ir.ClsW:
		return obj.DwarfType{Name: "int", Encoding: 5, Size: 4}
	case ir.ClsS:
		return obj.DwarfType{Name: "float", Encoding: 4, Size: 4}
	case ir.ClsD:
		return obj.DwarfType{Name: "double", Encoding: 4, Size: 8}
	default:
		return obj.DwarfType{Name: "long", Encoding: 5, Size: 8}
	}
}

// dwarfRetType returns the DWARF return type; an aggregate return is a pointer.
func dwarfRetType(f *ir.Func) obj.DwarfType {
	if f.RetAgg != nil {
		return dwarfType(ir.ClsL)
	}
	return dwarfType(f.Retty)
}

// dwarfParams captures each parameter as a DWARF formal parameter (an aggregate
// parameter is a pointer, class L). Called before lowering.
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

// paramTempIDs returns each parameter's temporary id, captured before lowering
// (ids are stable across it).
func paramTempIDs(f *ir.Func) []int {
	ids := make([]int, len(f.Params))
	for i, p := range f.Params {
		ids[i] = p.ID
	}
	return ids
}

// tempInterval returns a temporary's live range, or nil.
func (m *mc) tempInterval(id int) *interval {
	for _, iv := range m.alloc.intervals {
		if iv.temp == id {
			return iv
		}
	}
	return nil
}

// pcRange maps an interval (in the allocator's numbering) to the [lo, hi) PC
// range spanned by its instructions, taking min start / max end so it is robust
// to block-emission order.
func (m *mc) pcRange(start, end int) (lo, hi uint64, ok bool) {
	pi := m.alloc.posInstr
	lo = ^uint64(0)
	for q := start; q <= end && q < len(pi); q++ {
		if q < 0 {
			continue
		}
		if in := pi[q]; in != nil {
			if pc, o := m.instrPC[in]; o {
				if pc[0] < lo {
					lo = pc[0]
				}
				if pc[1] > hi {
					hi = pc[1]
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
		expr = obj.LocFrameBase(int64(-(m.spillBase + 8 + t.Slot)))
	}
	return &obj.VarLoc{Lo: lo, Hi: hi, Expr: expr}
}

// dwarfGP maps the native GP encoding (rax=0..r15=15) to its x86-64 DWARF number.
var dwarfGP = [16]uint32{0, 2, 1, 3, 7, 6, 4, 5, 8, 9, 10, 11, 12, 13, 14, 15}

// dwarfRegNum maps a physical register to its x86-64 DWARF register number
// (rax=0, rdx=1, rcx=2, rbx=3, rsi=4, rdi=5, rbp=6, rsp=7, r8..r15=8..15,
// xmm0..xmm15=17..32).
func dwarfRegNum(r Reg) uint32 {
	if r >= XMM0 {
		return 17 + uint32(r-XMM0)
	}
	return dwarfGP[r]
}

// inlineNode is an inlined region under construction.
type inlineNode struct {
	site     *ir.InlineSite
	lo, hi   uint64
	children []*inlineNode
}

// buildInlineTree turns inline-context samples into a nested tree of
// inlined-subroutine ranges. Samples mark where the context changes; the final
// context runs to size. Sites are interned, so compared by pointer.
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
			c[i], c[j] = c[j], c[i]
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
		for len(stack) > k {
			top := stack[len(stack)-1]
			top.hi = s.off
			stack = stack[:len(stack)-1]
		}
		for _, site := range chain[k:] {
			n := &inlineNode{site: site, lo: s.off}
			addChild(n)
			stack = append(stack, n)
		}
	}
	for len(stack) > 0 {
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

// stackMapFunc groups one function's safepoints under its symbol.
type stackMapFunc struct {
	sym    string
	points []safepoint
}

// setStackMap builds the .cg12_stackmaps section: a table of safepoint return
// addresses (each an ABS64 relocation to a function symbol plus the return
// offset) and the live GC-root locations at each. A collector finds it via the
// __cg12_stackmaps symbol. Format (little-endian): magic "SMAP", version u16=2,
// reserved u16, count u32, then per safepoint { pc u64 (relocated), nroots u32,
// roots[ kind u8, reserved u8[3], value i32, type u32 ] }.
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
				Offset: uint64(len(b)), Sym: f.sym, Type: obj.R_X86_64_64, Addend: int64(sp.pc),
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

// frameLayout is the stack-frame plan for one function, shared by the machine and
// text emitters. All offsets are relative to RBP (which points at the saved RBP).
type frameLayout struct {
	calleeSaved []Reg             // callee-saved registers to preserve, in save order
	spillBase   int               // bytes below RBP where spill slots begin
	allocOff    map[*ir.Instr]int // each stack allocation's distance below RBP
	frame       int               // bytes subtracted from RSP (16-aligned)
	regSaveDist int               // variadic register save area, at [rbp - regSaveDist]
}

// computeFrame lays out a function's stack frame from its allocation.
func computeFrame(f *ir.Func, alloc *allocation) frameLayout {
	var lay frameLayout
	lay.allocOff = map[*ir.Instr]int{}

	// Collect the callee-saved registers the allocator actually used.
	used := map[Reg]bool{}
	for _, t := range f.Temps {
		if t.Reg != ir.NoReg && calleeSavedReg(Reg(t.Reg)) {
			used[Reg(t.Reg)] = true
		}
	}
	for r := range used {
		lay.calleeSaved = append(lay.calleeSaved, r)
	}
	sort.Slice(lay.calleeSaved, func(i, j int) bool { return lay.calleeSaved[i] < lay.calleeSaved[j] })

	calleeArea := 8 * len(lay.calleeSaved)
	lay.spillBase = calleeArea
	acc := calleeArea + alloc.spillBytes

	// Place each stack allocation below the spills.
	maxCall := 0
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op.IsAlloc() {
				align, size := allocShape(f, in)
				acc += size
				acc = roundUp(acc, align)
				lay.allocOff[in] = acc
			}
			if in.Op == ir.OCall && int(in.Aux) > maxCall {
				maxCall = int(in.Aux)
			}
		}
	}
	if f.Variadic {
		acc += vaRegSaveSz
		lay.regSaveDist = acc
	}
	lay.frame = roundUp(acc+maxCall, 16)
	return lay
}

// mc holds the state of emitting one function to machine code.
type mc struct {
	f      *ir.Func
	alloc  *allocation
	prog   *x64.Program
	relocs []obj.Reloc
	err    error

	frameLayout // the shared stack-frame plan

	gc GCStrategy // pluggable GC strategy, or nil

	blockDone bool // a tail call already emitted the block's exit; skip the terminator

	vaSeq int // counter for unique va_arg branch labels
	gcSeq int // counter for unique GC poll labels

	safepoints []safepoint // GC safepoints recorded during emission

	rows    []obj.LineRow           // DWARF line-table rows
	lastPos ir.SrcPos               // last emitted source position
	instrPC map[*ir.Instr][2]uint64 // each instruction's [start, end) PC
	inl     []inlSample             // inline-context change samples
	lastInl *ir.InlineSite          // last emitted inline context
}

// inlSample marks the PC offset at which the inline context changed.
type inlSample struct {
	off  uint64
	site *ir.InlineSite
}

// System V register save area geometry: 6 GP registers (8 bytes each) followed by
// 8 XMM registers (16 bytes each) = 176 bytes; va_arg's gp_offset spans [0,48)
// and fp_offset spans [48,176).
const (
	vaGPBytes   = 48
	vaRegSaveSz = 176
)

// rootLoc is where a live GC reference sits at a safepoint: a register (kind 0,
// val = register number) or a frame slot (kind 1, val = byte offset from rbp),
// plus its type descriptor.
type rootLoc struct {
	kind uint8
	val  int32
	typ  uint32
}

const (
	rootReg   uint8 = 0 // val is a physical register number
	rootFrame uint8 = 1 // val is a byte offset from the frame pointer (rbp)
	rootSP    uint8 = 2 // val is a byte offset from the stack pointer at the safepoint
)

// safepoint is a call site's return address (function-relative) and the GC roots
// live across it.
type safepoint struct {
	pc    uint64
	roots []rootLoc
}

type machineCode struct {
	code       []byte
	relocs     []obj.Reloc
	safepoints []safepoint
	rows       []obj.LineRow
	inl        []inlSample
	blockSyms  []blockSym // local symbols for address-taken blocks (&&label in data)
	m          *mc        // retained so callers can query variable locations
}

// blockSym is a local symbol placed at a block's byte offset within its function.
type blockSym struct {
	name string
	off  int
}

func emitMachine(f *ir.Func, alloc *allocation, gc GCStrategy) (*machineCode, error) {
	m := &mc{f: f, alloc: alloc, gc: gc, prog: x64.NewProgram(), instrPC: map[*ir.Instr][2]uint64{}}
	m.planFrame()
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
	return &machineCode{code: code, relocs: m.relocs, safepoints: m.safepoints, rows: m.rows, inl: m.inl, blockSyms: blockSyms, m: m}, nil
}

// recordInline samples the inline context whenever it changes, keyed by PC. A
// nil site (returning to non-inlined code) is also recorded, to close regions.
func (m *mc) recordInline(site *ir.InlineSite) {
	if site == m.lastInl {
		return
	}
	m.lastInl = site
	m.inl = append(m.inl, inlSample{off: uint64(m.prog.Len()), site: site})
}

// recordLoc appends a DWARF line-table row when the source position changes.
func (m *mc) recordLoc(pos ir.SrcPos) {
	if !pos.Valid() || pos == m.lastPos {
		return
	}
	m.lastPos = pos
	m.rows = append(m.rows, obj.LineRow{
		TextOff: uint64(m.prog.Len()), File: int(pos.File), Line: int(pos.Line), Col: int(pos.Col),
	})
}

// recordSafepoint captures the GC roots live at the current PC (a call's return
// address, or an explicit OSafepoint). Roots forced to the stack by the allocator
// are frame-relative (offset from rbp); any in registers are recorded by number.
func (m *mc) recordSafepoint(in *ir.Instr) {
	roots := m.alloc.safeRoots[in]
	if len(roots) == 0 {
		return
	}
	locs := make([]rootLoc, 0, len(roots))
	for _, id := range roots {
		t := m.f.Temps[id]
		if t.Reg != ir.NoReg {
			locs = append(locs, rootLoc{kind: rootReg, val: int32(Reg(t.Reg).mreg()), typ: t.GCType})
		} else {
			locs = append(locs, rootLoc{kind: rootFrame, val: int32(-(m.spillBase + 8 + t.Slot)), typ: t.GCType})
		}
	}
	m.safepoints = append(m.safepoints, safepoint{pc: uint64(m.prog.Len()), roots: locs})
}

func (m *mc) fail(err error) {
	if m.err == nil {
		m.err = err
	}
}

func (m *mc) emit(b []byte) int { return m.prog.Emit(b) }

// recordReloc notes a relocation to be applied to the function's text at off.
// The symbol name is sanitized to match how symbols are registered (a raw name
// like "t.static.2" is registered as "t_static_2"); sanitize is idempotent, so
// already-clean names are unaffected.
func (m *mc) recordReloc(off int, sym string, typ uint32, addend int64) {
	m.relocs = append(m.relocs, obj.Reloc{Offset: uint64(off), Sym: sanitize(sym), Type: typ, Addend: addend})
}

// --- frame layout ----------------------------------------------------------

func allocShape(f *ir.Func, in *ir.Instr) (align, size int) {
	switch in.Op {
	case ir.OAlloc4:
		align = 4
	case ir.OAlloc8:
		align = 8
	default:
		align = 16
	}
	size = align
	if a := in.Arg(0); a.Kind == ir.RefConst {
		if c := f.Consts[a.ID]; c.Kind == ir.ConstInt && c.Int > 0 {
			size = int(c.Int)
		}
	}
	return align, roundUp(size, align)
}

func (m *mc) planFrame() { m.frameLayout = computeFrame(m.f, m.alloc) }

// slotAddr returns the RBP-relative address of spill slot s.
func (l *frameLayout) slotAddr(s int) int32 { return int32(-(l.spillBase + 8 + s)) }

// savedAddr returns the RBP-relative address of the k-th saved callee register.
func (l *frameLayout) savedAddr(k int) int32 { return int32(-8 * (k + 1)) }

func (m *mc) prologue() {
	// A strategy may emit a stack-growth guard before the frame is set up; its slow
	// path branches back to this label to re-check after the stack grows.
	if pe, ok := m.gc.(PrologueEmitter); ok {
		m.prog.Label("__cg12_prologue")
		pe.EmitPrologue(&PrologueContext{mc: m, retry: "__cg12_prologue"})
	}
	m.emit(x64.Push(RBP.mreg()))
	m.emit(x64.MovReg(true, RBP.mreg(), RSP.mreg()))
	if m.frame > 0 {
		m.emit(x64.SubImm(true, RSP.mreg(), int32(m.frame)))
	}
	for k, r := range m.calleeSaved {
		m.emit(x64.Store(64, r.mreg(), x64.At(RBP.mreg(), m.savedAddr(k))))
	}
	if m.f.Variadic {
		m.saveVarargRegs()
	}
}

// saveVarargRegs spills the argument registers to the register save area so
// va_arg can read the unnamed arguments that arrived in registers. It runs in the
// prologue, before the parameter shuffle overwrites those registers.
func (m *mc) saveVarargRegs() {
	base := int32(-m.regSaveDist)
	for i, r := range argGP {
		m.emit(x64.Store(64, r.mreg(), x64.At(RBP.mreg(), base+int32(8*i))))
	}
	for i, r := range argFP {
		m.emit(x64.MovsdStore(r.mreg(), x64.At(RBP.mreg(), base+int32(vaGPBytes+16*i))))
	}
}

// teardown restores callee-saved registers and unwinds the frame (mov rsp,rbp;
// pop rbp), leaving rsp at the return address without returning. It is shared by
// the return epilogue and the tail-call branch.
func (m *mc) teardown() {
	for k, r := range m.calleeSaved {
		m.emit(x64.Load(true, r.mreg(), x64.At(RBP.mreg(), m.savedAddr(k))))
	}
	m.emit(x64.MovReg(true, RSP.mreg(), RBP.mreg()))
	m.emit(x64.Pop(RBP.mreg()))
}

func (m *mc) epilogue() {
	m.teardown()
	m.emit(x64.Ret())
}

// --- location abstraction --------------------------------------------------

type locKind uint8

const (
	locReg locKind = iota
	locMem
	locImm
	locSym
)

// loc is a value's home: a register, a memory cell (base+off), an immediate, or
// a symbol address. size is the operand width in bytes (4 or 8).
type loc struct {
	kind   locKind
	reg    Reg
	base   Reg
	off    int32
	val    int64
	sym    string
	symoff int64
	size   int
	float  bool
	tls    bool // symbol is thread-local (addressed via the TLS ABI)
}

func regLoc(r Reg, size int, float bool) loc {
	return loc{kind: locReg, reg: r, size: size, float: float}
}
func memLoc(base Reg, off int32, size int, float bool) loc {
	return loc{kind: locMem, base: base, off: off, size: size, float: float}
}

// refLoc returns the home of an IR operand.
func (m *mc) refLoc(r ir.Ref) loc {
	switch r.Kind {
	case ir.RefTemp:
		t := m.f.Temps[r.ID]
		size := t.Cls.Size()
		fl := t.Cls.IsFloat()
		if t.Reg != ir.NoReg {
			return regLoc(Reg(t.Reg), size, fl)
		}
		return memLoc(RBP, m.slotAddr(t.Slot), size, fl)
	case ir.RefConst:
		c := m.f.Consts[r.ID]
		switch c.Kind {
		case ir.ConstInt:
			return loc{kind: locImm, val: c.Int, size: c.Cls.Size()}
		case ir.ConstFloat:
			if c.Cls == ir.ClsS {
				return loc{kind: locImm, val: int64(math.Float32bits(float32(c.Flt))), size: 4, float: true}
			}
			return loc{kind: locImm, val: int64(math.Float64bits(c.Flt)), size: 8, float: true}
		case ir.ConstSym:
			return loc{kind: locSym, sym: c.Sym, symoff: c.Int, size: 8, tls: c.Thread}
		}
	}
	m.fail(fmt.Errorf("amd64: unsupported operand ref kind %d", r.Kind))
	return loc{}
}

func w64(size int) bool { return size == 8 }

// move emits a single move that places src into dst.
func (m *mc) move(dst, src loc) {
	switch dst.kind {
	case locReg:
		m.moveToReg(dst, src)
	case locMem:
		m.moveToMem(dst, src)
	default:
		m.fail(fmt.Errorf("amd64: move to non-lvalue"))
	}
}

func (m *mc) moveToReg(dst, src loc) {
	switch src.kind {
	case locReg:
		if dst.reg == src.reg {
			return
		}
		if dst.float {
			if w64(src.size) {
				m.emit(x64.MovsdReg(dst.reg.mreg(), src.reg.mreg()))
			} else {
				m.emit(x64.MovssReg(dst.reg.mreg(), src.reg.mreg()))
			}
		} else {
			m.emit(x64.MovReg(w64(dst.size), dst.reg.mreg(), src.reg.mreg()))
		}
	case locMem:
		mem := x64.At(src.base.mreg(), src.off)
		if dst.float {
			if w64(src.size) {
				m.emit(x64.MovsdLoad(dst.reg.mreg(), mem))
			} else {
				m.emit(x64.MovssLoad(dst.reg.mreg(), mem))
			}
		} else {
			m.emit(x64.Load(w64(src.size), dst.reg.mreg(), mem))
		}
	case locImm:
		if dst.float {
			m.materializeFloat(dst.reg, src.val, src.size)
		} else {
			m.movImm(dst.reg, src.val, w64(dst.size))
		}
	case locSym:
		m.materializeSym(dst.reg, src.sym, src.symoff, src.tls)
	}
}

func (m *mc) moveToMem(dst, src loc) {
	mem := x64.At(dst.base.mreg(), dst.off)
	switch src.kind {
	case locReg:
		if src.float {
			if w64(dst.size) {
				m.emit(x64.MovsdStore(src.reg.mreg(), mem))
			} else {
				m.emit(x64.MovssStore(src.reg.mreg(), mem))
			}
		} else {
			m.emit(x64.Store(dst.size*8, src.reg.mreg(), mem))
		}
	case locMem:
		scratch := gpScratch0
		m.moveToReg(regLoc(scratch, src.size, src.float), src)
		m.moveToMem(dst, regLoc(scratch, dst.size, dst.float))
	case locImm:
		if dst.float {
			m.materializeFloat(fpScratch0, src.val, src.size)
			m.moveToMem(dst, regLoc(fpScratch0, dst.size, true))
		} else {
			m.movImm(gpScratch0, src.val, w64(dst.size))
			m.emit(x64.Store(dst.size*8, gpScratch0.mreg(), mem))
		}
	case locSym:
		m.materializeSym(gpScratch0, src.sym, src.symoff, src.tls)
		m.emit(x64.Store(64, gpScratch0.mreg(), mem))
	}
}

// movImm loads an integer immediate into a register.
func (m *mc) movImm(d Reg, val int64, w bool) {
	if !w {
		m.emit(x64.MovImm32(false, d.mreg(), int32(val)))
		return
	}
	if val >= math.MinInt32 && val <= math.MaxInt32 {
		m.emit(x64.MovImm32(true, d.mreg(), int32(val)))
		return
	}
	m.emit(x64.MovImm64(d.mreg(), val))
}

// materializeFloat loads a float constant (given by its bit pattern) into an XMM.
func (m *mc) materializeFloat(d Reg, bits int64, size int) {
	if size == 8 {
		m.movImm(gpScratch0, bits, true)
		m.emit(x64.MovqToXmm(true, d.mreg(), gpScratch0.mreg()))
	} else {
		m.movImm(gpScratch0, bits, false)
		m.emit(x64.MovqToXmm(false, d.mreg(), gpScratch0.mreg()))
	}
}

// materializeSym loads a symbol address into a register. An ordinary symbol uses
// a RIP-relative LEA + PC32 relocation; a thread-local symbol uses the local-exec
// model: load the thread pointer from %fs:0, then add its TP-relative offset.
func (m *mc) materializeSym(d Reg, sym string, off int64, tls bool) {
	if tls {
		m.emit(x64.MovFSZero(d.mreg()))         // d = thread pointer
		m.emit(x64.AddImm32(true, d.mreg(), 0)) // d += tpoff(sym) + off
		m.recordReloc(m.prog.Len()-4, sym, obj.R_X86_64_TPOFF32, off)
		return
	}
	m.emit(x64.Lea(true, d.mreg(), x64.RIPRel(0)))
	m.recordReloc(m.prog.Len()-4, sym, obj.R_X86_64_PC32, off-4)
}

// --- operand helpers -------------------------------------------------------

// gpValue returns a GPR holding ref's value, loading into scratch if needed.
func (m *mc) gpValue(ref ir.Ref, scratch Reg) Reg {
	l := m.refLoc(ref)
	if l.kind == locReg {
		return l.reg
	}
	m.moveToReg(regLoc(scratch, l.size, false), l)
	return scratch
}

// fpValue returns an XMM holding ref's value, loading into scratch if needed.
func (m *mc) fpValue(ref ir.Ref, scratch Reg) Reg {
	l := m.refLoc(ref)
	if l.kind == locReg {
		return l.reg
	}
	m.moveToReg(regLoc(scratch, l.size, true), l)
	return scratch
}

// gpInto places ref's value into GPR d.
func (m *mc) gpInto(d Reg, ref ir.Ref) {
	l := m.refLoc(ref)
	m.moveToReg(regLoc(d, l.size, false), l)
}

// fpInto places ref's value into XMM d.
func (m *mc) fpInto(d Reg, ref ir.Ref) {
	l := m.refLoc(ref)
	m.moveToReg(regLoc(d, l.size, true), l)
}

// gpDst returns the destination GPR for an int result and a commit closure that
// stores it back when the result is spilled.
func (m *mc) gpDst(ref ir.Ref) (Reg, func()) {
	t := m.f.Temps[ref.ID]
	if t.Reg != ir.NoReg {
		return Reg(t.Reg), func() {}
	}
	size := t.Cls.Size()
	return gpScratch0, func() {
		m.emit(x64.Store(size*8, gpScratch0.mreg(), x64.At(RBP.mreg(), m.slotAddr(t.Slot))))
	}
}

// fpDst is gpDst for a float result.
func (m *mc) fpDst(ref ir.Ref) (Reg, func()) {
	t := m.f.Temps[ref.ID]
	if t.Reg != ir.NoReg {
		return Reg(t.Reg), func() {}
	}
	size := t.Cls.Size()
	return fpScratch0, func() {
		mem := x64.At(RBP.mreg(), m.slotAddr(t.Slot))
		if size == 8 {
			m.emit(x64.MovsdStore(fpScratch0.mreg(), mem))
		} else {
			m.emit(x64.MovssStore(fpScratch0.mreg(), mem))
		}
	}
}

// constOf returns the constant a ref names, or nil.
func (m *mc) constOf(r ir.Ref) *ir.Const {
	if r.Kind == ir.RefConst {
		return &m.f.Consts[r.ID]
	}
	return nil
}

// --- parallel moves (argument/parameter shuffles) --------------------------

type locPair struct{ dst, src loc }

func sameLoc(a, b loc) bool {
	if a.kind == locReg && b.kind == locReg {
		return a.reg == b.reg
	}
	if a.kind == locMem && b.kind == locMem {
		return a.base == b.base && a.off == b.off
	}
	return false
}

// srcReadsDst reports whether reading src touches destination location dst.
func srcReadsDst(src, dst loc) bool {
	switch src.kind {
	case locReg:
		return dst.kind == locReg && src.reg == dst.reg
	case locMem:
		return dst.kind == locMem && src.base == dst.base && src.off == dst.off
	}
	return false
}

// parallelMove performs a set of simultaneous moves, ordering them so no source
// is clobbered before it is read and breaking register cycles with a scratch.
func (m *mc) parallelMove(pairs []locPair) {
	var work []locPair
	for _, p := range pairs {
		if !sameLoc(p.dst, p.src) {
			work = append(work, p)
		}
	}
	for len(work) > 0 {
		idx := -1
		for i, p := range work {
			blocked := false
			for j, q := range work {
				if i != j && srcReadsDst(q.src, p.dst) {
					blocked = true
					break
				}
			}
			if !blocked {
				idx = i
				break
			}
		}
		if idx >= 0 {
			m.move(work[idx].dst, work[idx].src)
			work = append(work[:idx], work[idx+1:]...)
			continue
		}
		// Cyclic: rescue a register destination into scratch and reroute readers.
		ci := -1
		for i, p := range work {
			if p.dst.kind == locReg {
				ci = i
				break
			}
		}
		if ci < 0 {
			m.fail(fmt.Errorf("amd64: unexpected memory cycle in parallel move"))
			return
		}
		saved := work[ci].dst
		rescue := gpScratch0
		if saved.float {
			rescue = fpScratch0
		}
		tmp := regLoc(rescue, saved.size, saved.float)
		m.move(tmp, saved)
		for i := range work {
			if srcReadsDst(work[i].src, saved) {
				work[i].src = tmp
			}
		}
	}
}

// --- block emission --------------------------------------------------------

func (m *mc) block(b *ir.Block) {
	i := 0
	if b == m.f.Start {
		var pairs []locPair
		var aggParams []*ir.Instr
		for i < len(b.Instrs) && b.Instrs[i].Op == ir.OPar {
			in := &b.Instrs[i]
			if t := m.f.Temps[in.To.ID]; t.Agg != nil {
				// A MEMORY/stack aggregate parameter: the param is the address of the
				// incoming bytes, resolved after the register shuffle.
				aggParams = append(aggParams, in)
				i++
				continue
			}
			dst := m.refLoc(in.To)
			var src loc
			if len(in.Args) > 0 {
				src = m.refLoc(in.Args[0])
			} else {
				src = memLoc(RBP, int32(16+in.Aux), in.Cls.Size(), in.Cls.IsFloat())
			}
			pairs = append(pairs, locPair{dst, src})
			i++
		}
		m.parallelMove(pairs)
		for _, in := range aggParams {
			m.leaStackParam(in.To, int32(16+in.Aux))
		}
	}

	m.blockDone = false
	var argPending []*ir.Instr
	for ; i < len(b.Instrs); i++ {
		in := &b.Instrs[i]
		start := uint64(m.prog.Len())
		m.recordLoc(in.Pos)
		m.recordInline(in.Inl)
		switch in.Op {
		case ir.OArg:
			argPending = append(argPending, in)
		case ir.OCall:
			m.emitArgs(argPending)
			// System V requires AL = number of vector registers used, for variadic
			// callees. Setting it before every call is harmless (rax is not an
			// argument register) and lets us call variadic functions without knowing
			// the callee's prototype.
			nfloat := 0
			for _, ai := range argPending {
				if !ai.To.IsNone() && ai.Cls.IsFloat() {
					nfloat++
				}
			}
			argPending = nil
			m.emit(x64.MovImm32(false, RAX.mreg(), int32(nfloat)))
			if in.Tail {
				m.emitTailCall(in)
				m.blockDone = true
			} else {
				m.emitCall(in)
				m.recordSafepoint(in) // the return address is a safepoint
			}
		default:
			m.instr(in)
		}
		m.instrPC[in] = [2]uint64{start, uint64(m.prog.Len())}
	}
	m.term(b)
}

// leaStackParam materializes the address of an aggregate parameter's incoming
// bytes ([rbp+off]) into the parameter temporary.
func (m *mc) leaStackParam(to ir.Ref, off int32) {
	dst := m.refLoc(to)
	if dst.kind == locReg {
		m.emit(x64.Lea(true, dst.reg.mreg(), x64.At(RBP.mreg(), off)))
		return
	}
	m.emit(x64.Lea(true, gpScratch0.mreg(), x64.At(RBP.mreg(), off)))
	m.emit(x64.Store(64, gpScratch0.mreg(), x64.At(RBP.mreg(), dst.off)))
}

func (m *mc) emitArgs(args []*ir.Instr) {
	var pairs []locPair
	for _, ai := range args {
		var dst loc
		if ai.To.IsNone() {
			dst = memLoc(RSP, int32(ai.Aux), ai.Cls.Size(), ai.Cls.IsFloat())
		} else {
			dst = m.refLoc(ai.To)
		}
		pairs = append(pairs, locPair{dst, m.refLoc(ai.Args[0])})
	}
	m.parallelMove(pairs)
}

// emitTailCall tears down this frame and jumps to the callee, which returns
// directly to our caller (frame reuse — the basis for guaranteed TCO). Register
// arguments are already in place; an indirect callee is staged in r11, which
// survives the teardown (it restores only callee-saved registers and rbp/rsp).
func (m *mc) emitTailCall(in *ir.Instr) {
	callee := in.Args[0]
	if c := m.constOf(callee); c != nil && c.Kind == ir.ConstSym {
		m.teardown()
		m.emit(x64.JmpRel(0))
		m.recordReloc(m.prog.Len()-4, c.Sym, obj.R_X86_64_PLT32, c.Int-4)
		return
	}
	r := m.gpValue(callee, gpScratch1)
	if r != gpScratch1 {
		m.emit(x64.MovReg(true, gpScratch1.mreg(), r.mreg()))
	}
	m.teardown()
	m.emit(x64.JmpReg(gpScratch1.mreg()))
}

func (m *mc) emitCall(in *ir.Instr) {
	(&xsel{f: m.f, b: &mcXasm{m: m}}).call(in)
}

func (m *mc) term(b *ir.Block) {
	if m.blockDone {
		return // a tail call already emitted this block's exit
	}
	if (&xsel{f: m.f, b: &mcXasm{m: m}}).term(b) {
		return
	}
	switch b.Jmp.Kind {
	case ir.JmpRet:
		m.epilogue()
	default:
		m.fail(fmt.Errorf("amd64: block %q has an unsupported terminator %d", b.Name, b.Jmp.Kind))
	}
}

// --- instruction selection -------------------------------------------------

func (m *mc) instr(in *ir.Instr) {
	// Two-operand integer arithmetic is selected once, through the shared builder.
	if (&xsel{f: m.f, b: &mcXasm{m: m}}).selectInt(in) {
		return
	}
	switch in.Op {
	case ir.OAlloc4, ir.OAlloc8, ir.OAlloc16:
		d, commit := m.gpDst(in.To)
		m.emit(x64.Lea(true, d.mreg(), x64.At(RBP.mreg(), int32(-m.allocOff[in]))))
		commit()
	case ir.OAsm:
		m.fail(fmt.Errorf("amd64: inline assembly is only supported when emitting assembly text, not object code"))
	case ir.OVaStart:
		m.vaStart(in)
	case ir.OVaArg:
		m.vaArg(in)
	case ir.OSafepoint:
		// Let the strategy emit poll code, then record the roots at the resulting PC.
		if m.gc != nil {
			m.gc.EmitSafepoint(&GCContext{mc: m, roots: m.gcRoots(in)})
		}
		m.recordSafepoint(in)
	default:
		m.fail(fmt.Errorf("amd64: unsupported op %s", in.Op))
	}
}

// memAddr resolves a load/store address operand to an x64 memory operand,
// handling a direct symbol (RIP-relative) or a computed pointer in a register.
// It returns the operand and, for the symbol case, records the PC32 relocation
// after the caller emits the instruction (via the returned fixup).
func (m *mc) memAddr(addr ir.Ref, scratch Reg) (x64.Mem, func()) {
	if c := m.constOf(addr); c != nil && c.Kind == ir.ConstSym && !c.Thread {
		sym, off := c.Sym, c.Int
		return x64.RIPRel(0), func() {
			m.recordReloc(m.prog.Len()-4, sym, obj.R_X86_64_PC32, off-4)
		}
	}
	// A thread-local symbol or a computed pointer resolves to a register first.
	r := m.gpValue(addr, scratch)
	return x64.At(r.mreg(), 0), func() {}
}
