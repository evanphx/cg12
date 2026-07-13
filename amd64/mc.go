package amd64

import (
	"fmt"
	"math"
	"sort"

	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// Options is reserved for future object-emission hooks (such as a pluggable GC
// strategy for safepoint code). DWARF and GC stack maps are emitted automatically.
type Options struct{}

// CompileObject compiles a module straight to ELF x86-64 relocatable-object
// bytes with the machine-code emitter (no external assembler).
func CompileObject(m *ir.Module) ([]byte, error) {
	o, err := CompileToObject(m)
	if err != nil {
		return nil, err
	}
	return o.MarshalELF()
}

// CompileToObject compiles a module to an in-memory relocatable object.
func CompileToObject(m *ir.Module) (*obj.Object, error) {
	o := &obj.Object{Machine: obj.EM_X86_64}
	var smFuncs []stackMapFunc
	var rows []obj.LineRow
	var dfuncs []obj.DwarfFunc
	anchor := ""
	for _, f := range m.Funcs {
		name := sanitize(f.Name)
		params := dwarfParams(f) // captured before lowering rewrites the params
		ir.LowerPointers(f, ir.ClsL)
		if err := lower(f); err != nil {
			return nil, fmt.Errorf("function %s: %w", f.Name, err)
		}
		alloc, err := regAlloc(f)
		if err != nil {
			return nil, fmt.Errorf("function %s: %w", f.Name, err)
		}
		mc, err := emitMachine(f, alloc)
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
		df := obj.DwarfFunc{
			Name: name, Sym: name, Size: uint64(len(mc.code)),
			HasRet: f.HasRet, RetType: dwarfRetType(f), Params: params, External: f.Linkage.Export,
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
		o.SetDWARF(m.Files, rows, dfuncs, uint64(len(o.Text)), anchor, "cg12", ".", m.Files[0], obj.R_X86_64_64)
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

	// Frame layout, all relative to RBP (which points at the saved RBP).
	calleeSaved []Reg             // callee-saved registers to preserve, in save order
	spillBase   int               // bytes below RBP where spill slots begin
	allocOff    map[*ir.Instr]int // each stack allocation's distance below RBP
	frame       int               // bytes subtracted from RSP (16-aligned)

	blockDone bool // a tail call already emitted the block's exit; skip the terminator

	// Variadic support: the System V register save area (rdi..r9 then xmm0..xmm7),
	// at [rbp - regSaveDist].
	regSaveDist int
	vaSeq       int // counter for unique va_arg branch labels

	safepoints []safepoint // GC safepoints recorded during emission

	rows    []obj.LineRow // DWARF line-table rows
	lastPos ir.SrcPos     // last emitted source position
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
	rootReg   uint8 = 0
	rootFrame uint8 = 1
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
}

func emitMachine(f *ir.Func, alloc *allocation) (*machineCode, error) {
	m := &mc{f: f, alloc: alloc, prog: x64.NewProgram(), allocOff: map[*ir.Instr]int{}}
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
	return &machineCode{code: code, relocs: m.relocs, safepoints: m.safepoints, rows: m.rows}, nil
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
func (m *mc) recordReloc(off int, sym string, typ uint32, addend int64) {
	m.relocs = append(m.relocs, obj.Reloc{Offset: uint64(off), Sym: sym, Type: typ, Addend: addend})
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

func (m *mc) planFrame() {
	// Collect the callee-saved registers the allocator actually used.
	used := map[Reg]bool{}
	for _, t := range m.f.Temps {
		if t.Reg != ir.NoReg && calleeSavedReg(Reg(t.Reg)) {
			used[Reg(t.Reg)] = true
		}
	}
	for r := range used {
		m.calleeSaved = append(m.calleeSaved, r)
	}
	sort.Slice(m.calleeSaved, func(i, j int) bool { return m.calleeSaved[i] < m.calleeSaved[j] })

	calleeArea := 8 * len(m.calleeSaved)
	m.spillBase = calleeArea
	acc := calleeArea + m.alloc.spillBytes

	// Place each stack allocation below the spills.
	maxCall := 0
	for _, b := range m.f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op.IsAlloc() {
				align, size := allocShape(m.f, in)
				acc += size
				acc = roundUp(acc, align)
				m.allocOff[in] = acc
			}
			if in.Op == ir.OCall && int(in.Aux) > maxCall {
				maxCall = int(in.Aux)
			}
		}
	}
	if m.f.Variadic {
		acc += vaRegSaveSz
		m.regSaveDist = acc
	}
	m.frame = roundUp(acc+maxCall, 16)
}

// slotAddr returns the RBP-relative address of spill slot s.
func (m *mc) slotAddr(s int) int32 { return int32(-(m.spillBase + 8 + s)) }

// savedAddr returns the RBP-relative address of the k-th saved callee register.
func (m *mc) savedAddr(k int) int32 { return int32(-8 * (k + 1)) }

func (m *mc) prologue() {
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
		m.recordLoc(in.Pos)
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
	callee := in.Args[0]
	if c := m.constOf(callee); c != nil && c.Kind == ir.ConstSym {
		m.emit(x64.CallRel(0))
		m.recordReloc(m.prog.Len()-4, c.Sym, obj.R_X86_64_PLT32, c.Int-4)
		return
	}
	r := m.gpValue(callee, gpScratch0)
	m.emit(x64.CallReg(r.mreg()))
}

func (m *mc) term(b *ir.Block) {
	if m.blockDone {
		return // a tail call already emitted this block's exit
	}
	switch b.Jmp.Kind {
	case ir.JmpJmp:
		m.prog.Jmp(b.Jmp.To.Name)
	case ir.JmpJnz:
		r := m.gpValue(b.Jmp.Arg, gpScratch0)
		w := m.f.ClassOf(b.Jmp.Arg) == ir.ClsL
		m.emit(x64.TestReg(w, r.mreg(), r.mreg()))
		m.prog.Jcc(x64.NE, b.Jmp.To.Name)
		m.prog.Jmp(b.Jmp.To2.Name)
	case ir.JmpRet:
		m.epilogue()
	case ir.JmpHlt:
		m.emit(x64.Ud2())
	}
}

// --- instruction selection -------------------------------------------------

func (m *mc) instr(in *ir.Instr) {
	switch in.Op {
	case ir.OAdd, ir.OSub, ir.OMul, ir.OAnd, ir.OOr, ir.OXor:
		if in.Cls.IsFloat() {
			m.binFP(in)
		} else {
			m.binInt(in)
		}
	case ir.ODiv:
		if in.Cls.IsFloat() {
			m.binFP(in)
		} else {
			m.divInt(in, true, false)
		}
	case ir.OUDiv:
		m.divInt(in, false, false)
	case ir.ORem:
		m.divInt(in, true, true)
	case ir.OURem:
		m.divInt(in, false, true)
	case ir.OShl, ir.OShr, ir.OSar:
		m.shift(in)
	case ir.ONeg:
		m.neg(in)
	case ir.OCmp:
		m.cmp(in)
	case ir.OCopy:
		m.move(m.refLoc(in.To), m.refLoc(in.Arg(0)))
	case ir.OStoreb, ir.OStoreh, ir.OStorew, ir.OStorel, ir.OStores, ir.OStored:
		m.store(in)
	case ir.OLoadsb, ir.OLoadub, ir.OLoadsh, ir.OLoaduh, ir.OLoadsw, ir.OLoaduw, ir.OLoadl, ir.OLoads, ir.OLoadd:
		m.load(in)
	case ir.OAlloc4, ir.OAlloc8, ir.OAlloc16:
		d, commit := m.gpDst(in.To)
		m.emit(x64.Lea(true, d.mreg(), x64.At(RBP.mreg(), int32(-m.allocOff[in]))))
		commit()
	case ir.OExtsb, ir.OExtub, ir.OExtsh, ir.OExtuh, ir.OExtsw, ir.OExtuw:
		m.extend(in)
	case ir.OExts, ir.OTruncd, ir.OStosi, ir.OStoui, ir.OSltof, ir.OUltof, ir.OCast:
		m.convert(in)
	case ir.OGetReg:
		// Read a physical register directly (Args[0] is a RefReg naming it).
		src := regLoc(Reg(in.Arg(0).ID), in.Cls.Size(), in.Cls.IsFloat())
		m.move(m.refLoc(in.To), src)
	case ir.OSetReg:
		// Write a value (Args[0]) to a physical register (Args[1] is a RefReg).
		dst := regLoc(Reg(in.Arg(1).ID), in.Cls.Size(), in.Cls.IsFloat())
		m.move(dst, m.refLoc(in.Arg(0)))
	case ir.OVaStart:
		m.vaStart(in)
	case ir.OVaArg:
		m.vaArg(in)
	case ir.OSafepoint:
		// No machine code; record the live GC roots at this PC.
		m.recordSafepoint(in)
	default:
		m.fail(fmt.Errorf("amd64: unsupported op %s", in.Op))
	}
}

// binInt emits an integer binary op: To = Args[0] op Args[1].
func (m *mc) binInt(in *ir.Instr) {
	w := in.Cls == ir.ClsL
	d, commit := m.gpDst(in.To)
	rb := m.gpValue(in.Arg(1), gpScratch1)
	if rb == d {
		m.emit(x64.MovReg(w, gpScratch1.mreg(), rb.mreg()))
		rb = gpScratch1
	}
	m.gpInto(d, in.Arg(0))
	switch in.Op {
	case ir.OAdd:
		m.emit(x64.AddReg(w, d.mreg(), rb.mreg()))
	case ir.OSub:
		m.emit(x64.SubReg(w, d.mreg(), rb.mreg()))
	case ir.OMul:
		m.emit(x64.Imul(w, d.mreg(), rb.mreg()))
	case ir.OAnd:
		m.emit(x64.AndReg(w, d.mreg(), rb.mreg()))
	case ir.OOr:
		m.emit(x64.OrReg(w, d.mreg(), rb.mreg()))
	case ir.OXor:
		m.emit(x64.XorReg(w, d.mreg(), rb.mreg()))
	}
	commit()
}

// binFP emits a float binary op: To = Args[0] op Args[1].
func (m *mc) binFP(in *ir.Instr) {
	dbl := in.Cls == ir.ClsD
	d, commit := m.fpDst(in.To)
	rb := m.fpValue(in.Arg(1), fpScratch1)
	if rb == d {
		if dbl {
			m.emit(x64.MovsdReg(fpScratch1.mreg(), rb.mreg()))
		} else {
			m.emit(x64.MovssReg(fpScratch1.mreg(), rb.mreg()))
		}
		rb = fpScratch1
	}
	m.fpInto(d, in.Arg(0))
	dm, rm := d.mreg(), rb.mreg()
	switch {
	case in.Op == ir.OAdd && dbl:
		m.emit(x64.Addsd(dm, rm))
	case in.Op == ir.OAdd:
		m.emit(x64.Addss(dm, rm))
	case in.Op == ir.OSub && dbl:
		m.emit(x64.Subsd(dm, rm))
	case in.Op == ir.OSub:
		m.emit(x64.Subss(dm, rm))
	case in.Op == ir.OMul && dbl:
		m.emit(x64.Mulsd(dm, rm))
	case in.Op == ir.OMul:
		m.emit(x64.Mulss(dm, rm))
	case in.Op == ir.ODiv && dbl:
		m.emit(x64.Divsd(dm, rm))
	case in.Op == ir.ODiv:
		m.emit(x64.Divss(dm, rm))
	default:
		m.fail(fmt.Errorf("amd64: unsupported float op %s", in.Op))
	}
	commit()
}

// divInt emits signed/unsigned division or remainder using RDX:RAX / r-m.
func (m *mc) divInt(in *ir.Instr, signed, rem bool) {
	w := in.Cls == ir.ClsL
	rb := m.gpValue(in.Arg(1), gpScratch1) // divisor (never RAX/RDX: those are reserved)
	m.gpInto(RAX, in.Arg(0))               // dividend low half
	if signed {
		if w {
			m.emit(x64.Cqo())
		} else {
			m.emit(x64.Cdq())
		}
		m.emit(x64.Idiv(w, rb.mreg()))
	} else {
		m.emit(x64.XorReg(w, RDX.mreg(), RDX.mreg()))
		m.emit(x64.Div(w, rb.mreg()))
	}
	d, commit := m.gpDst(in.To)
	res := RAX
	if rem {
		res = RDX
	}
	m.emit(x64.MovReg(w, d.mreg(), res.mreg()))
	commit()
}

// shift emits a variable or immediate shift: To = Args[0] shift Args[1].
func (m *mc) shift(in *ir.Instr) {
	w := in.Cls == ir.ClsL
	d, commit := m.gpDst(in.To)
	if c := m.constOf(in.Arg(1)); c != nil && c.Kind == ir.ConstInt {
		m.gpInto(d, in.Arg(0))
		amt := byte(c.Int & 63)
		switch in.Op {
		case ir.OShl:
			m.emit(x64.ShlImm(w, d.mreg(), amt))
		case ir.OShr:
			m.emit(x64.ShrImm(w, d.mreg(), amt))
		case ir.OSar:
			m.emit(x64.SarImm(w, d.mreg(), amt))
		}
		commit()
		return
	}
	// Variable count: it must live in CL. Load it before writing the destination
	// (the count's source register may be the destination register).
	m.gpInto(RCX, in.Arg(1))
	m.gpInto(d, in.Arg(0))
	switch in.Op {
	case ir.OShl:
		m.emit(x64.ShlCL(w, d.mreg()))
	case ir.OShr:
		m.emit(x64.ShrCL(w, d.mreg()))
	case ir.OSar:
		m.emit(x64.SarCL(w, d.mreg()))
	}
	commit()
}

// neg emits integer or float negation.
func (m *mc) neg(in *ir.Instr) {
	if in.Cls.IsFloat() {
		dbl := in.Cls == ir.ClsD
		d, commit := m.fpDst(in.To)
		m.fpInto(d, in.Arg(0))
		if dbl {
			m.movImm(gpScratch0, int64(-9223372036854775808), true) // 0x8000000000000000
			m.emit(x64.MovqToXmm(true, fpScratch1.mreg(), gpScratch0.mreg()))
			m.emit(x64.Xorpd(d.mreg(), fpScratch1.mreg()))
		} else {
			m.movImm(gpScratch0, 0x80000000, false)
			m.emit(x64.MovqToXmm(false, fpScratch1.mreg(), gpScratch0.mreg()))
			m.emit(x64.Xorps(d.mreg(), fpScratch1.mreg()))
		}
		commit()
		return
	}
	w := in.Cls == ir.ClsL
	d, commit := m.gpDst(in.To)
	m.gpInto(d, in.Arg(0))
	m.emit(x64.Neg(w, d.mreg()))
	commit()
}

// cmp emits a comparison producing a 0/1 result in To.
func (m *mc) cmp(in *ir.Instr) {
	if in.Cmp.IsFloat() {
		m.fcmp(in)
		return
	}
	argW := m.f.ClassOf(in.Arg(0)) == ir.ClsL
	ra := m.gpValue(in.Arg(0), gpScratch0)
	rb := m.gpValue(in.Arg(1), gpScratch1)
	m.emit(x64.CmpReg(argW, ra.mreg(), rb.mreg()))
	d, commit := m.gpDst(in.To)
	m.emit(x64.Setcc(intCond(in.Cmp), d.mreg()))
	m.emit(x64.MovzxByte(false, d.mreg(), d.mreg()))
	commit()
}

// intCond maps an integer predicate (flags from a - b) to a condition code.
func intCond(c ir.Cmp) x64.Cond {
	switch c {
	case ir.CmpEq:
		return x64.E
	case ir.CmpNe:
		return x64.NE
	case ir.CmpSlt:
		return x64.L
	case ir.CmpSle:
		return x64.LE
	case ir.CmpSgt:
		return x64.G
	case ir.CmpSge:
		return x64.GE
	case ir.CmpUlt:
		return x64.B
	case ir.CmpUle:
		return x64.BE
	case ir.CmpUgt:
		return x64.A
	case ir.CmpUge:
		return x64.AE
	}
	return x64.E
}

// fcmp emits a floating-point comparison producing a 0/1 result in To. ucomisd
// sets CF/ZF/PF; unordered (NaN) sets all three, which the ordered predicates
// must treat as false.
func (m *mc) fcmp(in *ir.Instr) {
	dbl := m.f.ClassOf(in.Arg(0)) == ir.ClsD
	a := m.fpValue(in.Arg(0), fpScratch0)
	b := m.fpValue(in.Arg(1), fpScratch1)
	ucomi := func(x, y Reg) {
		if dbl {
			m.emit(x64.Ucomisd(x.mreg(), y.mreg()))
		} else {
			m.emit(x64.Ucomiss(x.mreg(), y.mreg()))
		}
	}
	d, commit := m.gpDst(in.To)
	dm := d.mreg()
	switch in.Cmp {
	case ir.CmpFgt:
		ucomi(a, b)
		m.emit(x64.Setcc(x64.A, dm))
	case ir.CmpFge:
		ucomi(a, b)
		m.emit(x64.Setcc(x64.AE, dm))
	case ir.CmpFlt: // a < b  ==  b > a (ordered), unordered false
		ucomi(b, a)
		m.emit(x64.Setcc(x64.A, dm))
	case ir.CmpFle:
		ucomi(b, a)
		m.emit(x64.Setcc(x64.AE, dm))
	case ir.CmpFo:
		ucomi(a, b)
		m.emit(x64.Setcc(x64.NP, dm))
	case ir.CmpFuo:
		ucomi(a, b)
		m.emit(x64.Setcc(x64.P, dm))
	case ir.CmpFeq: // ZF=1 and ordered (PF=0)
		ucomi(a, b)
		m.emit(x64.Setcc(x64.NP, gpScratch0.mreg()))
		m.emit(x64.Setcc(x64.E, dm))
		m.emit(x64.AndReg(false, dm, gpScratch0.mreg()))
	case ir.CmpFne: // ZF=0 or unordered (PF=1)
		ucomi(a, b)
		m.emit(x64.Setcc(x64.P, gpScratch0.mreg()))
		m.emit(x64.Setcc(x64.NE, dm))
		m.emit(x64.OrReg(false, dm, gpScratch0.mreg()))
	default:
		m.fail(fmt.Errorf("amd64: unsupported float compare %v", in.Cmp))
	}
	m.emit(x64.MovzxByte(false, dm, dm))
	commit()
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

func (m *mc) load(in *ir.Instr) {
	mem, fixup := m.memAddr(in.Arg(0), gpScratch1)
	if in.Op == ir.OLoads || in.Op == ir.OLoadd {
		d, commit := m.fpDst(in.To)
		if in.Op == ir.OLoadd {
			m.emit(x64.MovsdLoad(d.mreg(), mem))
		} else {
			m.emit(x64.MovssLoad(d.mreg(), mem))
		}
		fixup()
		commit()
		return
	}
	d, commit := m.gpDst(in.To)
	w := in.Cls == ir.ClsL
	dm := d.mreg()
	switch in.Op {
	case ir.OLoadub:
		m.emit(x64.MovzxLoadByte(w, dm, mem))
	case ir.OLoadsb:
		m.emit(x64.MovsxLoadByte(w, dm, mem))
	case ir.OLoaduh:
		m.emit(x64.MovzxLoadWord(w, dm, mem))
	case ir.OLoadsh:
		m.emit(x64.MovsxLoadWord(w, dm, mem))
	case ir.OLoaduw:
		m.emit(x64.Load(false, dm, mem)) // 32-bit load zero-extends
	case ir.OLoadsw:
		m.emit(x64.MovsxdLoad(dm, mem))
	case ir.OLoadl:
		m.emit(x64.Load(true, dm, mem))
	}
	fixup()
	commit()
}

func (m *mc) store(in *ir.Instr) {
	// Args[0] is the value, Args[1] the address.
	if in.Op == ir.OStores || in.Op == ir.OStored {
		v := m.fpValue(in.Arg(0), fpScratch0)
		mem, fixup := m.memAddr(in.Arg(1), gpScratch1)
		if in.Op == ir.OStored {
			m.emit(x64.MovsdStore(v.mreg(), mem))
		} else {
			m.emit(x64.MovssStore(v.mreg(), mem))
		}
		fixup()
		return
	}
	v := m.gpValue(in.Arg(0), gpScratch0)
	mem, fixup := m.memAddr(in.Arg(1), gpScratch1)
	switch in.Op {
	case ir.OStoreb:
		m.emit(x64.Store(8, v.mreg(), mem))
	case ir.OStoreh:
		m.emit(x64.Store(16, v.mreg(), mem))
	case ir.OStorew:
		m.emit(x64.Store(32, v.mreg(), mem))
	case ir.OStorel:
		m.emit(x64.Store(64, v.mreg(), mem))
	}
	fixup()
}

func (m *mc) extend(in *ir.Instr) {
	w := in.Cls == ir.ClsL
	rs := m.gpValue(in.Arg(0), gpScratch1)
	d, commit := m.gpDst(in.To)
	dm, sm := d.mreg(), rs.mreg()
	switch in.Op {
	case ir.OExtsb:
		m.emit(x64.MovsxByte(w, dm, sm))
	case ir.OExtub:
		m.emit(x64.MovzxByte(w, dm, sm))
	case ir.OExtsh:
		m.emit(x64.MovsxWord(w, dm, sm))
	case ir.OExtuh:
		m.emit(x64.MovzxWord(w, dm, sm))
	case ir.OExtsw:
		m.emit(x64.Movsxd(dm, sm))
	case ir.OExtuw:
		m.emit(x64.MovReg(false, dm, sm)) // 32-bit mov zero-extends
	}
	commit()
}

func (m *mc) convert(in *ir.Instr) {
	switch in.Op {
	case ir.OExts: // single -> double
		rs := m.fpValue(in.Arg(0), fpScratch1)
		d, commit := m.fpDst(in.To)
		m.emit(x64.Cvtss2sd(d.mreg(), rs.mreg()))
		commit()
	case ir.OTruncd: // double -> single
		rs := m.fpValue(in.Arg(0), fpScratch1)
		d, commit := m.fpDst(in.To)
		m.emit(x64.Cvtsd2ss(d.mreg(), rs.mreg()))
		commit()
	case ir.OStosi, ir.OStoui: // float -> integer (truncating)
		srcD := m.f.ClassOf(in.Arg(0)) == ir.ClsD
		w := in.Cls == ir.ClsL
		rs := m.fpValue(in.Arg(0), fpScratch1)
		d, commit := m.gpDst(in.To)
		if srcD {
			m.emit(x64.Cvttsd2si(w, d.mreg(), rs.mreg()))
		} else {
			m.emit(x64.Cvttss2si(w, d.mreg(), rs.mreg()))
		}
		commit()
	case ir.OSltof, ir.OUltof: // integer -> float
		dstD := in.Cls == ir.ClsD
		w := m.f.ClassOf(in.Arg(0)) == ir.ClsL
		rs := m.gpValue(in.Arg(0), gpScratch1)
		d, commit := m.fpDst(in.To)
		if dstD {
			m.emit(x64.Cvtsi2sd(w, d.mreg(), rs.mreg()))
		} else {
			m.emit(x64.Cvtsi2ss(w, d.mreg(), rs.mreg()))
		}
		commit()
	case ir.OCast: // bit reinterpret between equal-width int/float
		if in.Cls.IsFloat() {
			rs := m.gpValue(in.Arg(0), gpScratch1)
			d, commit := m.fpDst(in.To)
			m.emit(x64.MovqToXmm(in.Cls == ir.ClsD, d.mreg(), rs.mreg()))
			commit()
		} else {
			rs := m.fpValue(in.Arg(0), fpScratch1)
			d, commit := m.gpDst(in.To)
			m.emit(x64.MovqFromXmm(in.Cls == ir.ClsL, d.mreg(), rs.mreg()))
			commit()
		}
	}
}
