package amd64

import (
	"fmt"
	"math"

	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// Options controls object emission. GC supplies a pluggable garbage-collector
// strategy that emits safepoint (and optionally prologue) code late during
// emission; nil leaves safepoints as code-free stack-map markers.
type Options struct {
	GC GCStrategy

	// TLSModel selects how a thread-local's address is computed. The zero value is
	// local-exec, the only model amd64 emitted before there was a choice, so a
	// caller that does not set this gets byte-identical code. See tls.go for what
	// each model assumes and costs.
	TLSModel TLSModel
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
// addAliases emits a symbol for each __attribute__((alias)) at its target's own
// location, so both names resolve to the same code or data. The target must be
// defined in this object (the C rule for the attribute).
func addAliases(o *obj.Object, aliases []*ir.Alias) error {
	if len(aliases) == 0 {
		return nil
	}
	byName := map[string]obj.Sym{}
	for _, s := range o.Syms {
		byName[s.Name] = s
	}
	for _, a := range aliases {
		tgt, ok := byName[sanitize(a.Target)]
		if !ok {
			return fmt.Errorf("alias %q: target %q is not defined in this object", a.Name, a.Target)
		}
		o.Syms = append(o.Syms, obj.Sym{
			Name: sanitize(a.Name), Section: tgt.Section, Value: tgt.Value,
			Size: tgt.Size, Global: a.Export, Func: a.Func, TLS: tgt.TLS,
		})
	}
	return nil
}

// goABIUnsupported names the Go-ABI feature f asks for that this backend cannot
// provide, or returns nil when f is compilable. amd64 has no runtime-managed frame
// at all: no morestack prologue, no g-relative stack-guard check, no Go stack-map
// metadata. It nevertheless used to accept the annotation and emit an ordinary
// System V frame anyway, so a function whose stack the Go runtime is supposed to
// grow got one that simply overruns it -- a silent miscompile with err == nil.
// ManagedFrame is the flag that gates that machinery (arm64 keys both its
// morestack prologue and its Go stack maps on UsesManagedFrame), so it is the
// tripwire worth having, and it is the one rejected here.
//
// The Go internal calling convention is deliberately NOT rejected, even though
// amd64 does not implement ABIInternal's register assignment either. CallConv is
// not currently a trustworthy signal that a function needs Go's ABI: goc applies
// CallConvGoInternal unconditionally to closure-shaped functions -- function
// literals (goc/compile.go:10119), method-value wrappers (:9781) and funcvalue
// adapters (:10638) -- which then pass their environment through an ordinary
// fixed-register temporary (rdx via goc's closureRegister) rather than through the
// convention's register assignment. Those bodies are self-consistent System V code
// and are correct today; rejecting them catches no latent miscompile and instead
// breaks working code (measured: 14 goc corpus subtests that build natively on
// amd64). Check whether those frontend sites still over-apply the annotation
// before tightening this.
//
// NoSplit and SystemStack are likewise not rejected. Neither describes the frame
// on its own: they only tune the managed frame's stack-growth check (arm64 reads
// NoSplit only inside its UsesManagedFrame prologue branch, and SystemStack only
// within that check, to pick g.stackguard1 and runtime_morestackc). A platform-ABI,
// unmanaged function emits no such check, so the flags have nothing to change --
// and the C path already sets NoSplit benignly on plain platform-ABI functions
// (goc's runtime coverage dump, semantic assembly's NOSPLIT flag), which compile
// correctly today. Rejecting them would break working code without catching any
// wrong code.
func goABIUnsupported(f *ir.Func) error {
	if f.UsesManagedFrame() {
		return fmt.Errorf("amd64: unsupported managed frame")
	}
	return nil
}

// rejectGoABI fails the whole module before any of it is compiled. It runs up
// front rather than per function inside the emit loop because lowering rewrites
// each function in place: bailing out midway would leave the caller's module
// partly lowered.
func rejectGoABI(m *ir.Module) error {
	for _, f := range m.Funcs {
		if err := goABIUnsupported(f); err != nil {
			return fmt.Errorf("function %s: %w", f.Name, err)
		}
	}
	return nil
}

func CompileToObjectWith(m *ir.Module, opts Options) (*obj.Object, error) {
	// Every exported entry point (CompileObject, CompileObjectWith,
	// CompileToObject) and Backend.CompileModule funnels through here, so this is
	// the one place codegen can be reached from and the one place the guard needs
	// to sit.
	if err := rejectGoABI(m); err != nil {
		return nil, err
	}
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
		mc, err := emitMachine(f, alloc, opts.GC, opts.TLSModel)
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
		if err := addData(o, d); err != nil {
			return nil, err
		}
	}
	if err := addAliases(o, m.Aliases); err != nil {
		return nil, err
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

// mc holds the state of emitting one function to machine code.
type mc struct {
	f      *ir.Func
	alloc  *allocation
	prog   *x64.Program
	relocs []obj.Reloc
	err    error

	frameLayout // the shared stack-frame plan

	gc GCStrategy // pluggable GC strategy, or nil

	tlsModel TLSModel // how a thread-local's address is reached (see tls.go)

	blockDone bool      // a tail call already emitted the block's exit; skip the terminator
	nextBlock *ir.Block // block laid out after the current one, for fall-through elision
	useCount  []int     // per-temp use count, for the fused compare-branch

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

func emitMachine(f *ir.Func, alloc *allocation, gc GCStrategy, tlsModel TLSModel) (*machineCode, error) {
	m := &mc{f: f, alloc: alloc, gc: gc, tlsModel: tlsModel, prog: x64.NewProgram(), instrPC: map[*ir.Instr][2]uint64{}}
	m.planFrame()
	m.useCount = countTempUses(f)
	m.prologue()
	for i, b := range f.Blocks {
		m.prog.Label(b.Name)
		if i+1 < len(f.Blocks) {
			m.nextBlock = f.Blocks[i+1]
		} else {
			m.nextBlock = nil
		}
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

// --- prologue / epilogue ---------------------------------------------------

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
// the return epilogue and the tail-call branch, and selected once through xsel.
func (m *mc) teardown() {
	(&xsel{f: m.f, b: &mcXasm{m: m}}).teardown(&m.frameLayout)
}

func (m *mc) epilogue() {
	(&xsel{f: m.f, b: &mcXasm{m: m}}).epilogue(&m.frameLayout)
}

// --- location abstraction --------------------------------------------------

type locKind uint8

const (
	locReg locKind = iota
	locMem
	locImm
	locSym
	locFrameAddr // the ADDRESS base+off (an lea), not a load from it: a rematerialised alloca
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
		if m.alloc != nil {
			if rule, ok := m.alloc.remat[int(r.ID)]; ok {
				return m.rematLoc(rule, size, fl)
			}
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

// rematLoc is the location a rematerialisable temp resolves to: its source
// computation (an immediate, a symbol address, or a frame-slot address), which the
// move machinery then materialises at the point of use.
func (m *mc) rematLoc(rule rematRule, size int, fl bool) loc {
	switch rule.kind {
	case rematConst:
		return loc{kind: locImm, val: rule.c.Int, size: size, float: fl}
	case rematSym:
		return loc{kind: locSym, sym: rule.c.Sym, symoff: rule.c.Int, size: 8}
	case rematAlloca:
		return loc{kind: locFrameAddr, base: RBP, off: -int32(m.allocOff[rule.in]), size: 8}
	}
	m.fail(fmt.Errorf("amd64: unknown remat rule kind %d", rule.kind))
	return loc{}
}

// isRematDef reports whether in defines a rematerialised temp -- one recomputed at
// each use and given no register or slot -- so its defining instruction is skipped.
func (m *mc) isRematDef(in *ir.Instr) bool {
	if in.To.Kind != ir.RefTemp || m.alloc == nil {
		return false
	}
	if m.f.Temps[in.To.ID].Reg != ir.NoReg {
		return false
	}
	_, ok := m.alloc.remat[int(in.To.ID)]
	return ok
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
	case locFrameAddr:
		m.emit(x64.Lea(true, dst.reg.mreg(), x64.At(src.base.mreg(), src.off)))
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
	case locFrameAddr:
		m.emit(x64.Lea(true, gpScratch0.mreg(), x64.At(src.base.mreg(), src.off)))
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
// a RIP-relative LEA + PC32 relocation; a thread-local symbol goes through the
// TLS model the options selected, whose sequences live in tls.go.
func (m *mc) materializeSym(d Reg, sym string, off int64, tls bool) {
	if tls {
		m.materializeThreadSym(d, sym, off)
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

// parallelMove performs a set of simultaneous moves, selected once through the
// shared xsel, which owns the ordering logic.
func (m *mc) parallelMove(pairs []locPair) {
	(&xsel{f: m.f, b: &mcXasm{m: m}}).parallelMove(pairs)
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
	fuseCmp := m.fusableCmp(b)
	var argPending []*ir.Instr
	for ; i < len(b.Instrs); i++ {
		in := &b.Instrs[i]
		start := uint64(m.prog.Len())
		m.recordLoc(in.Pos)
		m.recordInline(in.Inl)
		if in == fuseCmp {
			// Emit the comparison as flags only (no setcc); term() branches on them.
			(&xsel{f: m.f, b: &mcXasm{m: m}}).cmpFlags(in)
			m.instrPC[in] = [2]uint64{start, uint64(m.prog.Len())}
			continue
		}
		if m.isRematDef(in) {
			// The value is recomputed at each use; its definition emits nothing.
			m.instrPC[in] = [2]uint64{start, uint64(m.prog.Len())}
			continue
		}
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
	x := &xsel{f: m.f, b: &mcXasm{m: m}, next: m.nextBlock}
	if fuse := m.fusableCmp(b); fuse != nil {
		x.fusedBranch(b, fuse)
		return
	}
	if x.term(b) {
		return
	}
	switch b.Jmp.Kind {
	case ir.JmpRet:
		m.epilogue()
	default:
		m.fail(fmt.Errorf("amd64: block %q has an unsupported terminator %d", b.Name, b.Jmp.Kind))
	}
}

// fusableCmp reports the block's integer comparison when it feeds only the block's
// own jnz terminator, so the comparison and branch fuse into `cmp; jcc` instead of
// materialising a boolean with setcc and then testing it. The comparison need not
// be the last instruction: SSA destruction can append flag-preserving copies after
// it. Returns nil when no fusion applies.
func (m *mc) fusableCmp(b *ir.Block) *ir.Instr {
	if b.Jmp.Kind != ir.JmpJnz || b.Jmp.Arg.Kind != ir.RefTemp {
		return nil
	}
	if m.useCount[b.Jmp.Arg.ID] != 1 { // the sole use must be the branch
		return nil
	}
	idx := -1
	for i := range b.Instrs {
		if in := &b.Instrs[i]; in.To.Kind == ir.RefTemp && in.To.ID == b.Jmp.Arg.ID {
			if in.Op != ir.OCmp || in.Cmp.IsFloat() {
				return nil // only an integer compare fuses to jcc
			}
			idx = i
		}
	}
	if idx < 0 {
		return nil
	}
	for i := idx + 1; i < len(b.Instrs); i++ {
		if b.Instrs[i].Op != ir.OCopy { // a copy preserves the flags; anything else may not
			return nil
		}
	}
	return &b.Instrs[idx]
}

// countTempUses counts, per temp, how many times it appears as an operand.
func countTempUses(f *ir.Func) []int {
	n := make([]int, len(f.Temps))
	bump := func(r ir.Ref) {
		if r.Kind == ir.RefTemp && int(r.ID) < len(n) {
			n[r.ID]++
		}
	}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			for _, a := range b.Instrs[k].Args {
				bump(a)
			}
		}
		bump(b.Jmp.Arg)
		for _, a := range b.Jmp.Args {
			bump(a)
		}
	}
	return n
}

// --- instruction selection -------------------------------------------------

func (m *mc) instr(in *ir.Instr) {
	// Two-operand integer arithmetic is selected once, through the shared builder.
	if (&xsel{f: m.f, b: &mcXasm{m: m}}).selectInt(in) {
		return
	}
	switch in.Op {
	case ir.ONop:
		// A dead address computation the addressing-mode fold consumed.
	case ir.OAlloc4, ir.OAlloc8, ir.OAlloc16:
		d, commit := m.gpDst(in.To)
		m.emit(x64.Lea(true, d.mreg(), x64.At(RBP.mreg(), int32(-m.allocOff[in]))))
		commit()
	case ir.OSpill:
		// Save a caller-saved value to its slot before a call it is live across.
		t := m.f.Temps[in.Args[0].ID]
		m.spillReg(Reg(t.Reg), int(in.Aux), t.Cls)
	case ir.OReload:
		// Restore it into the same register after the call.
		t := m.f.Temps[in.To.ID]
		m.reloadReg(Reg(t.Reg), int(in.Aux), t.Cls)
	case ir.OAsm:
		m.emitAsm(in)
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
	case ir.OLifeStart, ir.OLifeEnd:
		// Alloca lifetime markers carry no code; they only bound live ranges and
		// stack-slot lifetimes for earlier passes.
	default:
		m.fail(fmt.Errorf("amd64: unsupported op %s", in.Op))
	}
}

// spillReg stores a register to spill slot s; reloadReg loads it back. Used by the
// caller-save OSpill/OReload wrapping a call a value is live across.
func (m *mc) spillReg(r Reg, s int, cls ir.Cls) {
	at := x64.At(RBP.mreg(), m.slotAddr(s))
	switch {
	case cls.IsFloat() && cls.Size() == 8:
		m.emit(x64.MovsdStore(r.mreg(), at))
	case cls.IsFloat():
		m.emit(x64.MovssStore(r.mreg(), at))
	default:
		m.emit(x64.Store(cls.Size()*8, r.mreg(), at))
	}
}

func (m *mc) reloadReg(r Reg, s int, cls ir.Cls) {
	at := x64.At(RBP.mreg(), m.slotAddr(s))
	switch {
	case cls.IsFloat() && cls.Size() == 8:
		m.emit(x64.MovsdLoad(r.mreg(), at))
	case cls.IsFloat():
		m.emit(x64.MovssLoad(r.mreg(), at))
	default:
		m.emit(x64.Load(cls.Size() == 8, r.mreg(), at))
	}
}

// memFor builds the memory operand for a (possibly address-folded) load or store.
// ai is the index of the base among the instruction's args (0 for a load, 1 for a
// store, past the stored value). in.Aux is the displacement and in.Amode the index
// scale (0 = no index).
//
// One general-purpose scratch register is available to it, gpScratch1, and only
// that one. gpScratch0 is already spoken for by the callers: xselect.go's store
// resolves the stored value with gpValue(..., gpScratch0) before handing it to
// storeGP, so on a store of a spilled value gpScratch0 holds that value across the
// whole address computation, and gpDst likewise hands out gpScratch0 as the
// destination of a load whose result is spilled. So the operand is built to need at
// most gpScratch1:
//
//   - A base already in a register is used as it is, and a rematerialised alloca
//     base (locFrameAddr) folds into rbp + displacement. Neither costs a register,
//     so an index that needs loading takes gpScratch1.
//   - A base with no register of its own -- a spilled alloca, or the base+disp
//     shape's general pointer -- is loaded into gpScratch1. If an index needs
//     loading too there is no second register for it, so base + index*scale is
//     computed into gpScratch1 alone (addrIntoScratch) and the operand degrades to
//     [gpScratch1 + disp] rather than a SIB.
//
// That last case is the one this used to get wrong: it loaded the base into
// gpScratch1 and then the index into gpScratch1 as well, silently addressing
// [index + index*scale + disp]. It was reachable because an alloca whose address is
// passed to a call is not rematerialisable (remat.go's srcResolvesOperands), so a
// folded [alloca + index*scale] can have a base that lives in a spill slot after
// all -- the invariant this comment used to assert, that an index's base always
// resolves to rbp+off, was false.
func (m *mc) memFor(in *ir.Instr, ai int) (x64.Mem, func()) {
	base := in.Args[ai]
	disp := int32(in.Aux)
	if c := m.constOf(base); c != nil && c.Kind == ir.ConstSym && !c.Thread {
		sym, off := c.Sym, c.Int+int64(disp)
		return x64.RIPRel(0), func() {
			m.recordReloc(m.prog.Len()-4, sym, obj.R_X86_64_PC32, off-4)
		}
	}
	var mem x64.Mem
	mem.Disp = disp
	baseLoc := m.refLoc(base)
	if in.Amode == 0 {
		switch baseLoc.kind {
		case locFrameAddr:
			mem.Base, mem.Disp = baseLoc.base.mreg(), mem.Disp+baseLoc.off
		case locReg:
			mem.Base = baseLoc.reg.mreg()
		default:
			mem.Base = m.gpValue(base, gpScratch1).mreg()
		}
		return mem, func() {}
	}

	index := in.Args[ai+1]
	scale := byte(in.Amode)
	indexLoc := m.refLoc(index)
	// A pre-coloured temp could name gpScratch1 itself, in which case loading the
	// base there would clobber it, so "the index has a register" means a register
	// that is not the one the base would take.
	indexHasReg := indexLoc.kind == locReg && indexLoc.reg != gpScratch1
	switch {
	case baseLoc.kind == locFrameAddr:
		mem.Base, mem.Disp = baseLoc.base.mreg(), mem.Disp+baseLoc.off
	case baseLoc.kind == locReg:
		mem.Base = baseLoc.reg.mreg()
	case indexHasReg:
		// Only the base needs the scratch; loading it cannot disturb the index.
		mem.Base = m.gpValue(base, gpScratch1).mreg()
	default:
		m.addrIntoScratch(baseLoc, index, scale)
		mem.Base = gpScratch1.mreg()
		return mem, func() {}
	}
	mem.Index = m.gpValue(index, gpScratch1).mreg()
	mem.Scale = scale
	mem.HasIndex = true
	return mem, func() {}
}

// addrIntoScratch computes base + index*scale into gpScratch1 without touching any
// other register, for the one folded address that cannot be encoded directly: base
// and index both need loading, and gpScratch1 is the only register memFor may use.
//
// The index is loaded first because it is the operand that has to be scaled, and
// scaling it in place is a shift; the base then folds in from its home with a
// single memory-operand add. Base-first would leave the index unscaled with no
// register to scale it in -- the very register that is missing.
func (m *mc) addrIntoScratch(baseLoc loc, index ir.Ref, scale byte) {
	if baseLoc.kind != locMem {
		// Unreachable from foldAddressing, which pairs an index only with an alloca
		// base: an alloca temp's home is a register, a rematerialised frame address
		// (both handled by memFor without coming here), or a spill slot, which is
		// this locMem. An immediate or symbol-address base has no memory home to add
		// from and would need the second register this path exists to avoid, so it is
		// refused rather than encoded as something else.
		m.fail(fmt.Errorf("amd64: folded indexed address with a base of kind %d needs two scratch registers", baseLoc.kind))
		return
	}
	indexLoc := m.refLoc(index)
	m.moveToReg(regLoc(gpScratch1, indexLoc.size, false), indexLoc)
	// The scale multiplies the whole 64-bit register, exactly as the SIB byte's
	// scale would, so the shift is 64-bit even for a 32-bit index -- whose load
	// zero-extended it, so the high half is the zero the SIB form would have seen.
	if shift := scaleShift(scale); shift != 0 {
		m.emit(x64.ShlImm(true, gpScratch1.mreg(), shift))
	}
	m.emit(x64.AddMem(true, gpScratch1.mreg(), x64.At(baseLoc.base.mreg(), baseLoc.off)))
}

// scaleShift is the shift amount an index scale of 1, 2, 4 or 8 stands for, the
// shift form of what x64's scaleBits encodes into a SIB byte.
func scaleShift(scale byte) byte {
	switch scale {
	case 2:
		return 1
	case 4:
		return 2
	case 8:
		return 3
	default:
		return 0
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
	// A rematerialised alloca address is [rbp+off]; fold it straight into the
	// memory operand -- mov reg, [rbp+off] -- rather than materialising the address
	// with an lea into a register and dereferencing that.
	if addr.Kind == ir.RefTemp {
		if l := m.refLoc(addr); l.kind == locFrameAddr {
			return x64.At(l.base.mreg(), l.off), func() {}
		}
	}
	// A thread-local symbol or a computed pointer resolves to a register first.
	r := m.gpValue(addr, scratch)
	return x64.At(r.mreg(), 0), func() {}
}

// emitAsm binds an inline-asm template's operands to registers and assembles the
// result into machine code. The template is written for an assembler, so rather
// than teach the object path to render every instruction a user might write, we
// expand the operands and hand the text to x64.Assemble -- which drives the very
// encoders this file uses, so a template only reaches instructions we can encode
// and says so plainly when it does not.
func (m *mc) emitAsm(in *ir.Instr) {
	asm := in.Asm
	vals := make([]asmVal, len(asm.Ops))
	// Two pools, because an operand's register has to come from the file its class
	// lives in: a double in a GP register is not a double. They are counted
	// separately so a template using both does not exhaust one by spending the
	// other's budget.
	gp := [...]Reg{gpScratch0, gpScratch1}
	fp := [...]Reg{fpScratch0, fpScratch1}
	gpN, fpN := 0, 0
	next := func(float bool) Reg {
		if float {
			if fpN >= len(fp) {
				m.fail(fmt.Errorf("amd64: inline asm needs more XMM scratch registers than are available"))
				return fpScratch0
			}
			r := fp[fpN]
			fpN++
			return r
		}
		if gpN >= len(gp) {
			m.fail(fmt.Errorf("amd64: inline asm needs more scratch registers than are available"))
			return gpScratch0
		}
		r := gp[gpN]
		gpN++
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
		// The output is spilled: give the template a scratch register and store it
		// back once the template has run.
		float := m.f.ClassOf(oref).IsFloat()
		r := next(float)
		slot := m.slotAddr(t.Slot)
		finals = append(finals, func() {
			switch {
			case float && w == 8:
				m.emit(x64.MovsdStore(r.mreg(), x64.At(RBP.mreg(), slot)))
			case float:
				m.emit(x64.MovssStore(r.mreg(), x64.At(RBP.mreg(), slot)))
			default:
				m.emit(x64.Store(w*8, r.mreg(), x64.At(RBP.mreg(), slot)))
			}
		})
		return r, w
	}
	for i, kind := range asm.Ops {
		switch kind {
		case ir.AsmRegOut:
			r, w := resolveOut()
			vals[i] = asmVal{reg: r, width: w}
		case ir.AsmRegInOut:
			r, w := resolveOut()
			pre, _ := m.asmInputReg(in.Args[ac], next) // preload value
			ac++
			switch {
			case r.IsFloat() && w == 8:
				m.emit(x64.MovsdReg(r.mreg(), pre.mreg()))
			case r.IsFloat():
				m.emit(x64.MovssReg(r.mreg(), pre.mreg()))
			default:
				m.emit(x64.MovReg(w == 8, r.mreg(), pre.mreg()))
			}
			vals[i] = asmVal{reg: r, width: w}
		case ir.AsmImm:
			vals[i] = asmVal{lit: true, litS: fmt.Sprintf("$%d", m.f.Consts[in.Args[ac].ID].Int)}
			ac++
		case ir.AsmMem:
			r, _ := m.asmInputReg(in.Args[ac], next) // the operand's address
			vals[i] = asmVal{lit: true, litS: memn(r, 0)}
			ac++
		default: // AsmRegIn
			r, w := m.asmInputReg(in.Args[ac], next)
			vals[i] = asmVal{reg: r, width: w}
			ac++
		}
	}

	text, err := expandAsm(asm.Template, vals)
	if err != nil {
		m.fail(fmt.Errorf("amd64: %w", err))
		return
	}
	code, err := x64.Assemble(text)
	if err != nil {
		m.fail(fmt.Errorf("amd64: inline assembly %q: %w", text, err))
		return
	}
	m.emit(code)
	for _, f := range finals {
		f()
	}
}

// asmInputReg yields a register holding an inline-asm operand's value, loading a
// spilled temporary or materializing a constant into a scratch register first.
func (m *mc) asmInputReg(ref ir.Ref, next func(float bool) Reg) (Reg, int) {
	switch ref.Kind {
	case ir.RefTemp:
		t := m.f.Temps[ref.ID]
		cls := m.f.ClassOf(ref)
		w := cls.Size()
		if t.Reg != ir.NoReg {
			return Reg(t.Reg), w
		}
		r := next(cls.IsFloat())
		switch {
		case cls.IsFloat() && w == 8:
			m.emit(x64.MovsdLoad(r.mreg(), x64.At(RBP.mreg(), m.slotAddr(t.Slot))))
		case cls.IsFloat():
			m.emit(x64.MovssLoad(r.mreg(), x64.At(RBP.mreg(), m.slotAddr(t.Slot))))
		default:
			m.emit(x64.Load(w == 8, r.mreg(), x64.At(RBP.mreg(), m.slotAddr(t.Slot))))
		}
		return r, w
	case ir.RefConst:
		c := m.f.Consts[ref.ID]
		r := next(false) // an int or a symbol address: a GP register either way
		switch c.Kind {
		case ir.ConstInt:
			m.movImm(r, c.Int, true)
		case ir.ConstSym:
			m.materializeSym(r, c.Sym, c.Int, c.Thread)
		default:
			m.fail(fmt.Errorf("amd64: unsupported inline-asm constant operand"))
		}
		return r, 8
	}
	m.fail(fmt.Errorf("amd64: unsupported inline-asm operand %v", ref))
	return gpScratch0, 8
}
