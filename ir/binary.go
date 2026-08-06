package ir

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// The on-disk format lets a compiled unit (an optimized module) be cached to
// disk and reloaded, skipping the front-end and optimizer. It is a compact,
// versioned binary encoding: a magic tag and a version byte guard against stale
// or foreign caches (a mismatch is a decode error, so the caller recompiles).
//
// Pointer references — blocks, and aggregate types — are encoded as indices:
// blocks by position within their function, aggregate types by position in a
// per-module type table that also captures types reached only through
// references. Value references (ir.Ref) already index Temps/Consts by ID.
const (
	binMagic   = "cg12"
	binVersion = 21
)

// MarshalBinary encodes the module to cg12's binary unit format.
func (m *Module) MarshalBinary() ([]byte, error) {
	types, typeIdx := collectTypes(m)
	e := &enc{typeIdx: typeIdx}
	e.buf = append(e.buf, binMagic...)
	e.u8(binVersion)
	e.boolean(m.Runtime)
	e.str(m.GoModuleData)
	e.boolean(m.GoHasMain)

	// The full type table (for reference resolution), then which of them are the
	// module's declared types.
	e.uv(uint64(len(types)))
	for _, t := range types {
		e.encType(t)
	}
	e.uv(uint64(len(m.Types)))
	for _, t := range m.Types {
		e.uv(uint64(typeIdx[t]))
	}

	e.uv(uint64(len(m.Files)))
	for _, f := range m.Files {
		e.str(f)
	}
	// Frontend attachments: opaque payloads keyed by name. Assembly rides here
	// under its reserved key so the core format stays frontend-agnostic. Sorted
	// keys keep the encoding deterministic (cache correctness).
	attachments := map[string][]byte{}
	for key, value := range m.Attachments {
		attachments[key] = value
	}
	if len(m.Assembly) > 0 {
		attachments[asmAttachmentKey] = EncodeAssembly(m.Assembly)
	}
	attachmentKeys := make([]string, 0, len(attachments))
	for key := range attachments {
		attachmentKeys = append(attachmentKeys, key)
	}
	sort.Strings(attachmentKeys)
	e.uv(uint64(len(attachmentKeys)))
	for _, key := range attachmentKeys {
		e.str(key)
		e.str(string(attachments[key]))
	}
	e.uv(uint64(len(m.Data)))
	for _, d := range m.Data {
		e.encData(d)
	}
	e.uv(uint64(len(m.Funcs)))
	for _, f := range m.Funcs {
		e.encFunc(f)
	}
	return e.buf, nil
}

// DecodeModule decodes a module from cg12's binary unit format, and verifies it.
func DecodeModule(data []byte) (*Module, error) { return decodeModule(data, true) }

// DecodeModuleUnverified decodes without running [VerifyModule].
//
// It exists because the verifier currently rejects IR the front end really emits:
// 211 of 5083 functions on a whole-program Go module (4.2%) -- deferwraps,
// methodvalues and closures whose entry block reads a temporary nothing defines
// -- fail Verify straight out of the front end, before anything is encoded.
// BUILD_CACHE.md 2.6 records the same failure from the other side. Until that is
// fixed, a cache that gates on VerifyModule is gating on a check that is known to
// be wrong about one function in twenty-four, and it silently loses those
// functions rather than reporting a defect.
//
// A caller that skips the verifier owes an integrity check of its own. The memo's
// is a digest: it records the sha256 of the bytes it wrote and compares it to the
// bytes it read, which is a strictly stronger check against a truncated or
// corrupted unit than a structural verifier is, and does not depend on the
// verifier's model of anything.
func DecodeModuleUnverified(data []byte) (*Module, error) { return decodeModule(data, false) }

func decodeModule(data []byte, verify bool) (*Module, error) {
	if len(data) < len(binMagic)+1 || string(data[:len(binMagic)]) != binMagic {
		return nil, errors.New("ir: not a cg12 unit (bad magic)")
	}
	d := &dec{buf: data, pos: len(binMagic)}
	if v := d.u8(); v != binVersion {
		return nil, fmt.Errorf("ir: unit format version %d, want %d", v, binVersion)
	}

	m := NewModule()
	m.Runtime = d.boolean()
	m.GoModuleData = d.str()
	m.GoHasMain = d.boolean()
	nt := int(d.uv())
	types := make([]*AggType, nt)
	for i := range types {
		types[i] = &AggType{}
	}
	d.types = types
	for i := range types {
		d.decType(types[i])
	}
	nDecl := int(d.uv())
	m.Types = make([]*AggType, nDecl)
	for i := range m.Types {
		m.Types[i] = types[d.uv()]
	}

	m.Files = make([]string, int(d.uv()))
	for i := range m.Files {
		m.Files[i] = d.str()
	}
	attachmentCount := int(d.uv())
	for range attachmentCount {
		key := d.str()
		value := []byte(d.str())
		if key == asmAttachmentKey {
			assembly, err := DecodeAssembly(value)
			if err != nil {
				return nil, err
			}
			m.Assembly = assembly
			continue
		}
		if m.Attachments == nil {
			m.Attachments = map[string][]byte{}
		}
		m.Attachments[key] = value
	}
	m.Data = make([]*Data, int(d.uv()))
	for i := range m.Data {
		m.Data[i] = d.decData()
	}
	m.Funcs = make([]*Func, int(d.uv()))
	for i := range m.Funcs {
		m.Funcs[i] = d.decFunc(m)
	}
	if d.err != nil {
		return nil, d.err
	}
	// The format's promise is that a decoded module is the module. Bytes that
	// decode without complaint can still describe a function no builder would
	// make -- a truncated or hand-edited unit, or one written by a version that
	// carried a field this one does not.
	if verify {
		if err := VerifyModule(m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// collectTypes gathers every aggregate type reachable from the module — declared
// types plus those reached only through function/instruction/field references —
// assigning each a stable index for encoding.
func collectTypes(m *Module) ([]*AggType, map[*AggType]int) {
	var list []*AggType
	idx := make(map[*AggType]int)
	var add func(t *AggType)
	add = func(t *AggType) {
		if t == nil {
			return
		}
		if _, ok := idx[t]; ok {
			return
		}
		idx[t] = len(list)
		list = append(list, t)
		for _, f := range t.Fields {
			add(f.Type)
		}
		for _, c := range t.Cases {
			for _, f := range c {
				add(f.Type)
			}
		}
	}
	for _, t := range m.Types {
		add(t)
	}
	for _, f := range m.Funcs {
		add(f.RetAgg)
		for _, value := range f.AggregateValues {
			add(value.Type)
		}
		for _, group := range f.ParamGroups {
			add(group.Type)
		}
		for _, t := range f.Temps {
			if t != nil {
				add(t.Agg)
			}
		}
		for _, b := range f.Blocks {
			for i := range b.Instrs {
				in := &b.Instrs[i]
				add(in.RetAgg)
				for _, a := range in.AggArgs {
					add(a)
				}
				for _, group := range in.ArgGroups {
					add(group.Type)
				}
			}
		}
	}
	return list, idx
}

// sortedKeys orders a stack-pointer-word table's allocation ids, so the encoding
// of a function is a function of the function and not of Go's map iteration.
func sortedKeys(m map[uint32]map[int]bool) []uint32 {
	out := make([]uint32, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// sortedOffsets orders one allocation's pointer-word offsets, for the same
// reason.
func sortedOffsets(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for offset := range m {
		out = append(out, offset)
	}
	sort.Ints(out)
	return out
}

// --- encoder --------------------------------------------------------------

type enc struct {
	buf     []byte
	typeIdx map[*AggType]int
	// siteIndex numbers the inline sites of the function currently being
	// encoded, so an instruction can name one by index instead of by value.
	siteIndex map[*InlineSite]int
}

func (e *enc) u8(v byte)   { e.buf = append(e.buf, v) }
func (e *enc) uv(v uint64) { e.buf = binary.AppendUvarint(e.buf, v) }
func (e *enc) iv(v int64)  { e.buf = binary.AppendVarint(e.buf, v) }
func (e *enc) boolean(v bool) {
	if v {
		e.u8(1)
	} else {
		e.u8(0)
	}
}
func (e *enc) str(s string) { e.uv(uint64(len(s))); e.buf = append(e.buf, s...) }
func (e *enc) ref(r Ref)    { e.u8(byte(r.Kind)); e.uv(uint64(r.ID)) }

func (e *enc) f64(v float64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
	e.buf = append(e.buf, b[:]...)
}

// typeRef encodes an aggregate-type pointer as index+1, with 0 meaning nil.
func (e *enc) typeRef(t *AggType) {
	if t == nil {
		e.uv(0)
		return
	}
	e.uv(uint64(e.typeIdx[t] + 1))
}

func (e *enc) srcPos(p SrcPos) { e.uv(uint64(p.File)); e.uv(uint64(p.Line)); e.uv(uint64(p.Col)) }

func (e *enc) linkage(l Linkage) {
	e.boolean(l.Export)
	e.boolean(l.Thread)
	e.str(l.Section)
	e.str(l.SecArgs)
}

func (e *enc) valueGroups(groups []ValueGroup) {
	e.uv(uint64(len(groups)))
	for _, group := range groups {
		e.iv(int64(group.Index))
		e.iv(int64(group.Count))
		e.typeRef(group.Type)
	}
}

func (e *enc) encType(t *AggType) {
	e.str(t.Name)
	e.iv(int64(t.Align))
	e.iv(int64(t.Size))
	e.boolean(t.Opaque)
	e.boolean(t.Union)
	e.uv(uint64(len(t.Fields)))
	for _, f := range t.Fields {
		e.encField(f)
	}
	e.uv(uint64(len(t.Cases)))
	for _, c := range t.Cases {
		e.uv(uint64(len(c)))
		for _, f := range c {
			e.encField(f)
		}
	}
}

func (e *enc) encField(f Field) {
	e.u8(byte(f.Sub))
	e.typeRef(f.Type)
	e.iv(int64(f.Count))
	e.boolean(f.Pointer)
}

func (e *enc) encData(d *Data) {
	e.str(d.Name)
	e.linkage(d.Linkage)
	e.iv(int64(d.Align))
	e.boolean(d.GoTypeLink)
	e.uv(uint64(len(d.PointerWords)))
	for _, word := range d.PointerWords {
		e.iv(int64(word))
	}
	e.uv(uint64(len(d.Items)))
	for _, it := range d.Items {
		e.u8(byte(it.Sub))
		e.iv(int64(it.Zero))
		e.uv(uint64(len(it.Ints)))
		for _, v := range it.Ints {
			e.iv(v)
		}
		e.uv(uint64(len(it.Flts)))
		for _, v := range it.Flts {
			e.f64(v)
		}
		e.str(it.Sym)
		// RelativeTo is the difference between a 32-bit module-relative offset and
		// a full symbol address, and the backend takes a different relocation path
		// for each (arm64/mc.go). Every abi.Type goc emits carries several -- the
		// name offset, the type offset, each method's two offset words -- so a
		// format that dropped it demoted 1639 data words on a hello-world.
		e.str(it.RelativeTo)
		e.iv(it.Off)
		e.str(it.Str)
	}
}

func (e *enc) encFunc(f *Func) {
	e.str(f.Name)
	e.linkage(f.Linkage)
	e.boolean(f.HasRet)
	e.u8(byte(f.Retty))
	e.typeRef(f.RetAgg)
	e.boolean(f.RetValues)
	e.boolean(f.Variadic)
	e.u8(byte(f.CallConv))
	e.boolean(f.ManagedFrame)
	e.boolean(f.NoSplit)
	e.boolean(f.SystemStack)
	e.boolean(f.HasClosureContext)
	e.boolean(f.ForceInline)
	e.boolean(f.CostInline)
	e.boolean(f.NoInline)
	// StackPointerWords is what the collector scans an allocation by, and
	// InferStackPointerWords *adds* to it rather than rebuilding it -- so a
	// function that lost it in a round trip comes back with a smaller pointer map
	// and a smaller frame. Measured, decoding every function of the small program
	// and re-emitting: .text 43712 bytes smaller and .data 30344 bytes larger.
	// ir.CloneFunc round-trips through this encoder, so this was a real gap in
	// snapshot/restore before it was a gap in the memo.
	e.uv(uint64(len(f.StackPointerWords)))
	for _, id := range sortedKeys(f.StackPointerWords) {
		e.uv(uint64(id))
		words := f.StackPointerWords[id]
		e.uv(uint64(len(words)))
		for _, offset := range sortedOffsets(words) {
			e.iv(int64(offset))
		}
	}

	e.uv(uint64(len(f.Temps)))
	for _, t := range f.Temps {
		e.encTemp(t)
	}
	e.uv(uint64(len(f.Consts)))
	for _, c := range f.Consts {
		e.encConst(c)
	}
	e.uv(uint64(len(f.AggregateValues)))
	for _, value := range f.AggregateValues {
		e.typeRef(value.Type)
		e.uv(uint64(len(value.Parts)))
		for _, part := range value.Parts {
			e.ref(part)
		}
	}
	e.uv(uint64(len(f.Params)))
	for _, p := range f.Params {
		e.uv(uint64(p.ID)) // Params are Temps; ID is the index into Temps
	}
	e.valueGroups(f.ParamGroups)

	sites := e.collectInlineSites(f)
	e.uv(uint64(len(sites)))
	for _, s := range sites {
		e.str(s.Callee)
		e.srcPos(s.Call)
		if s.Parent == nil {
			e.uv(0)
		} else {
			e.uv(uint64(e.siteIndex[s.Parent] + 1))
		}
	}

	blockIdx := make(map[*Block]int, len(f.Blocks))
	for i, b := range f.Blocks {
		blockIdx[b] = i
	}
	blockRef := func(b *Block) {
		if b == nil {
			e.uv(0)
			return
		}
		e.uv(uint64(blockIdx[b] + 1))
	}

	e.uv(uint64(len(f.Blocks)))
	for _, b := range f.Blocks {
		e.encBlock(b, blockRef)
	}
	blockRef(f.Start)
}

func (e *enc) encTemp(t *Temp) {
	e.str(t.Name)
	e.u8(byte(t.Cls))
	e.iv(int64(t.Slot))
	e.iv(int64(t.Reg))
	e.boolean(t.Fixed)
	e.typeRef(t.Agg)
	e.boolean(t.GCRef)
	e.uv(uint64(t.GCType))
	e.boolean(t.ClosureContext)
}

func (e *enc) encConst(c Const) {
	e.u8(byte(c.Kind))
	e.u8(byte(c.Cls))
	e.iv(c.Int)
	e.f64(c.Flt)
	e.str(c.Sym)
}

func (e *enc) encBlock(b *Block, blockRef func(*Block)) {
	e.str(b.Name)
	e.str(b.Sym)
	e.iv(int64(b.ID))
	e.boolean(b.SecondaryEntry)
	e.uv(uint64(len(b.SyntheticSuccs)))
	for _, successor := range b.SyntheticSuccs {
		blockRef(successor)
	}
	e.srcPos(b.Pos)

	e.uv(uint64(len(b.Phis)))
	for _, p := range b.Phis {
		e.u8(byte(p.Cls))
		e.ref(p.To)
		e.uv(uint64(len(p.Args)))
		for _, a := range p.Args {
			e.ref(a)
		}
		e.uv(uint64(len(p.Blocks)))
		for _, bl := range p.Blocks {
			blockRef(bl)
		}
	}
	e.uv(uint64(len(b.Instrs)))
	for i := range b.Instrs {
		e.encInstr(&b.Instrs[i], blockRef)
	}
	// terminator
	e.u8(byte(b.Jmp.Kind))
	e.ref(b.Jmp.Arg)
	blockRef(b.Jmp.To)
	blockRef(b.Jmp.To2)
	e.uv(uint64(len(b.Jmp.Args)))
	for _, a := range b.Jmp.Args {
		e.ref(a)
	}
	e.uv(uint64(len(b.Jmp.Targets)))
	for _, t := range b.Jmp.Targets {
		blockRef(t)
	}
	e.boolean(b.Jmp.Signed)
	e.uv(uint64(len(b.Jmp.Cases)))
	for _, c := range b.Jmp.Cases {
		e.iv(c.Val)
		blockRef(c.Blk)
	}
}

func (e *enc) encInstr(in *Instr, blockRef func(*Block)) {
	e.uv(uint64(in.Op))
	e.u8(byte(in.Cls))
	e.ref(in.To)
	e.uv(uint64(len(in.Args)))
	for _, a := range in.Args {
		e.ref(a)
	}
	e.u8(byte(in.Cmp))
	e.iv(in.Aux)
	e.iv(int64(in.Unroll))
	e.u8(byte(in.CallConv))
	e.boolean(in.CallConvSet)
	e.iv(int64(in.Amode))
	e.uv(uint64(len(in.AggArgs)))
	for _, t := range in.AggArgs {
		e.typeRef(t)
	}
	e.valueGroups(in.ArgGroups)
	e.uv(uint64(len(in.Defs)))
	for _, r := range in.Defs {
		e.ref(r)
	}
	e.typeRef(in.RetAgg)
	e.boolean(in.RetValues)
	e.ref(in.StackResult)
	e.iv(in.StackResultOffset)
	e.srcPos(in.Pos)
	e.boolean(in.Tail)
	e.boolean(in.Volatile)
	e.boolean(in.ClosureCall)
	e.ref(in.ClosureContext)
	e.asmOp(in.Asm)
	e.intrinOp(in.Intrin)
	e.inlineSite(in.Inl)
	blockRef(in.Blk) // OBlockAddr target (nil for every other op)
}

// intrinOp encodes an OIntrinsic's dispatch name, or its absence.
func (e *enc) intrinOp(in *IntrinOp) {
	if in == nil {
		e.boolean(false)
		return
	}
	e.boolean(true)
	e.str(in.Name)
}

// asmOp encodes an inline-asm template and its operand kinds, or its absence.
func (e *enc) asmOp(a *AsmOp) {
	if a == nil {
		e.boolean(false)
		return
	}
	e.boolean(true)
	e.str(a.Template)
	e.boolean(a.ExactClobbers)
	e.uv(uint64(len(a.Ops)))
	for _, k := range a.Ops {
		e.u8(byte(k))
	}
	e.uv(uint64(len(a.Regs)))
	for _, r := range a.Regs {
		e.str(r)
	}
	e.uv(uint64(len(a.Clobbers)))
	for _, c := range a.Clobbers {
		e.str(c)
	}
}

// inlineSite encodes an inline context: a chain of call sites, innermost first.
// inlineSite writes an instruction's inline provenance as an index into the
// function's site table, which is what keeps two instructions from the same
// inline *sharing* a node across the round trip.
//
// Writing the chain by value, which is what this did, is faithful to the values
// and not to the graph, and the graph is what consumers read: arm64's
// buildInlineTree opens and closes a DWARF inlined-subroutine context on
// `stack[k].site == chain[k]`, pointer identity. Decoding gave every instruction
// its own chain, so one inlined region became one region per instruction --
// measured on the small program, .debug_info 3.4 MB -> 10.0 MB for identical
// code. Interning equal chains on decode fixes most of that and is still not the
// same graph: two splices of one callee at one position are distinct nodes, and
// merging them left .debug_info 3141 bytes short of the reference. A table of
// indices reproduces the sharing exactly.
func (e *enc) inlineSite(s *InlineSite) {
	if s == nil {
		e.uv(0)
		return
	}
	e.uv(uint64(e.siteIndex[s] + 1))
}

// collectInlineSites numbers every distinct site a function's instructions
// reference, parents before children, in a deterministic walk order.
func (e *enc) collectInlineSites(f *Func) []*InlineSite {
	e.siteIndex = map[*InlineSite]int{}
	var order []*InlineSite
	var add func(*InlineSite)
	add = func(s *InlineSite) {
		if s == nil {
			return
		}
		if _, seen := e.siteIndex[s]; seen {
			return
		}
		add(s.Parent) // a parent is numbered before the child that names it
		e.siteIndex[s] = len(order)
		order = append(order, s)
	}
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			add(b.Instrs[i].Inl)
		}
	}
	return order
}

// --- decoder --------------------------------------------------------------

type dec struct {
	buf   []byte
	pos   int
	err   error
	types []*AggType
	// sites is the site table of the function currently being decoded; an
	// instruction's inline provenance is an index into it.
	sites []*InlineSite
}

func (d *dec) valueGroups() []ValueGroup {
	groups := make([]ValueGroup, int(d.uv()))
	for index := range groups {
		groups[index] = ValueGroup{
			Index: int(d.iv()),
			Count: int(d.iv()),
			Type:  d.typeRef(),
		}
	}
	return groups
}

func (d *dec) fail(what string) {
	if d.err == nil {
		d.err = fmt.Errorf("ir: truncated unit decoding %s at offset %d", what, d.pos)
	}
}

func (d *dec) u8() byte {
	if d.pos >= len(d.buf) {
		d.fail("byte")
		return 0
	}
	v := d.buf[d.pos]
	d.pos++
	return v
}

func (d *dec) uv() uint64 {
	v, n := binary.Uvarint(d.buf[d.pos:])
	if n <= 0 {
		d.fail("uvarint")
		return 0
	}
	d.pos += n
	return v
}

func (d *dec) iv() int64 {
	v, n := binary.Varint(d.buf[d.pos:])
	if n <= 0 {
		d.fail("varint")
		return 0
	}
	d.pos += n
	return v
}

func (d *dec) boolean() bool { return d.u8() != 0 }

func (d *dec) f64() float64 {
	if d.pos+8 > len(d.buf) {
		d.fail("float64")
		return 0
	}
	v := binary.LittleEndian.Uint64(d.buf[d.pos:])
	d.pos += 8
	return math.Float64frombits(v)
}

func (d *dec) str() string {
	n := int(d.uv())
	if n < 0 || d.pos+n > len(d.buf) {
		d.fail("string")
		return ""
	}
	s := string(d.buf[d.pos : d.pos+n])
	d.pos += n
	return s
}

func (d *dec) ref() Ref { return Ref{Kind: RefKind(d.u8()), ID: uint32(d.uv())} }

func (d *dec) typeRef() *AggType {
	v := d.uv()
	if v == 0 || int(v) > len(d.types) {
		return nil
	}
	return d.types[v-1]
}

func (d *dec) srcPos() SrcPos {
	return SrcPos{File: uint32(d.uv()), Line: uint32(d.uv()), Col: uint32(d.uv())}
}

func (d *dec) linkage() Linkage {
	return Linkage{Export: d.boolean(), Thread: d.boolean(), Section: d.str(), SecArgs: d.str()}
}

func (d *dec) decType(t *AggType) {
	t.Name = d.str()
	t.Align = int(d.iv())
	t.Size = int(d.iv())
	t.Opaque = d.boolean()
	t.Union = d.boolean()
	t.Fields = make([]Field, int(d.uv()))
	for i := range t.Fields {
		t.Fields[i] = d.decField()
	}
	t.Cases = make([][]Field, int(d.uv()))
	for i := range t.Cases {
		t.Cases[i] = make([]Field, int(d.uv()))
		for j := range t.Cases[i] {
			t.Cases[i][j] = d.decField()
		}
	}
}

func (d *dec) decField() Field {
	return Field{Sub: SubCls(d.u8()), Type: d.typeRef(), Count: int(d.iv()), Pointer: d.boolean()}
}

func (d *dec) decData() *Data {
	dt := &Data{Name: d.str(), Linkage: d.linkage(), Align: int(d.iv())}
	dt.GoTypeLink = d.boolean()
	dt.PointerWords = make([]int, int(d.uv()))
	for i := range dt.PointerWords {
		dt.PointerWords[i] = int(d.iv())
	}
	dt.Items = make([]DataItem, int(d.uv()))
	for i := range dt.Items {
		it := DataItem{Sub: SubCls(d.u8()), Zero: int(d.iv())}
		it.Ints = make([]int64, int(d.uv()))
		for j := range it.Ints {
			it.Ints[j] = d.iv()
		}
		it.Flts = make([]float64, int(d.uv()))
		for j := range it.Flts {
			it.Flts[j] = d.f64()
		}
		it.Sym = d.str()
		it.RelativeTo = d.str()
		it.Off = d.iv()
		it.Str = d.str()
		dt.Items[i] = it
	}
	return dt
}

func (d *dec) decFunc(m *Module) *Func {
	f := &Func{mod: m}
	f.Name = d.str()
	f.Linkage = d.linkage()
	f.HasRet = d.boolean()
	f.Retty = Cls(d.u8())
	f.RetAgg = d.typeRef()
	f.RetValues = d.boolean()
	f.Variadic = d.boolean()
	f.CallConv = CallConvention(d.u8())
	f.ManagedFrame = d.boolean()
	f.NoSplit = d.boolean()
	f.SystemStack = d.boolean()
	f.HasClosureContext = d.boolean()
	f.ForceInline = d.boolean()
	f.CostInline = d.boolean()
	f.NoInline = d.boolean()
	if n := int(d.uv()); n > 0 {
		f.StackPointerWords = make(map[uint32]map[int]bool, n)
		for i := 0; i < n; i++ {
			id := uint32(d.uv())
			count := int(d.uv())
			words := make(map[int]bool, count)
			for j := 0; j < count; j++ {
				words[int(d.iv())] = true
			}
			f.StackPointerWords[id] = words
		}
	}

	f.Temps = make([]*Temp, int(d.uv()))
	for i := range f.Temps {
		f.Temps[i] = d.decTemp(i)
	}
	f.Consts = make([]Const, int(d.uv()))
	for i := range f.Consts {
		f.Consts[i] = d.decConst()
	}
	f.AggregateValues = make([]AggregateValue, int(d.uv()))
	for index := range f.AggregateValues {
		value := AggregateValue{Type: d.typeRef()}
		value.Parts = make([]Ref, int(d.uv()))
		for partIndex := range value.Parts {
			value.Parts[partIndex] = d.ref()
		}
		f.AggregateValues[index] = value
	}
	f.Params = make([]*Temp, int(d.uv()))
	for i := range f.Params {
		if id := int(d.uv()); id < len(f.Temps) {
			f.Params[i] = f.Temps[id]
		} else {
			d.fail("param temp index")
		}
	}
	f.ParamGroups = d.valueGroups()
	d.sites = d.decInlineSites()

	f.Blocks = make([]*Block, int(d.uv()))
	for i := range f.Blocks {
		f.Blocks[i] = &Block{fn: f}
	}
	blockRef := func() *Block {
		v := d.uv()
		if v == 0 || int(v) > len(f.Blocks) {
			return nil
		}
		return f.Blocks[v-1]
	}
	for _, b := range f.Blocks {
		d.decBlock(b, blockRef)
	}
	f.Start = blockRef()
	return f
}

func (d *dec) decTemp(id int) *Temp {
	t := &Temp{
		ID:    id,
		Name:  d.str(),
		Cls:   Cls(d.u8()),
		Slot:  int(d.iv()),
		Reg:   int(d.iv()),
		Fixed: d.boolean(),
		Agg:   d.typeRef(),
		GCRef: d.boolean(),
	}
	t.GCType = uint32(d.uv())
	t.ClosureContext = d.boolean()
	return t
}

func (d *dec) decConst() Const {
	return Const{
		Kind: ConstKind(d.u8()),
		Cls:  Cls(d.u8()),
		Int:  d.iv(),
		Flt:  d.f64(),
		Sym:  d.str(),
	}
}

func (d *dec) decBlock(b *Block, blockRef func() *Block) {
	b.Name = d.str()
	b.Sym = d.str()
	b.ID = int(d.iv())
	b.SecondaryEntry = d.boolean()
	b.SyntheticSuccs = make([]*Block, int(d.uv()))
	for index := range b.SyntheticSuccs {
		b.SyntheticSuccs[index] = blockRef()
	}
	b.Pos = d.srcPos()

	b.Phis = make([]*Phi, int(d.uv()))
	for i := range b.Phis {
		p := &Phi{Cls: Cls(d.u8()), To: d.ref()}
		p.Args = make([]Ref, int(d.uv()))
		for j := range p.Args {
			p.Args[j] = d.ref()
		}
		p.Blocks = make([]*Block, int(d.uv()))
		for j := range p.Blocks {
			p.Blocks[j] = blockRef()
		}
		b.Phis[i] = p
	}
	b.Instrs = make([]Instr, int(d.uv()))
	for i := range b.Instrs {
		b.Instrs[i] = d.decInstr(blockRef)
	}
	b.Jmp = Jmp{Kind: JmpKind(d.u8()), Arg: d.ref()}
	b.Jmp.To = blockRef()
	b.Jmp.To2 = blockRef()
	b.Jmp.Args = make([]Ref, int(d.uv()))
	for i := range b.Jmp.Args {
		b.Jmp.Args[i] = d.ref()
	}
	if n := int(d.uv()); n > 0 {
		b.Jmp.Targets = make([]*Block, n)
		for i := range b.Jmp.Targets {
			b.Jmp.Targets[i] = blockRef()
		}
	}
	b.Jmp.Signed = d.boolean()
	if n := int(d.uv()); n > 0 {
		b.Jmp.Cases = make([]SwitchCase, n)
		for i := range b.Jmp.Cases {
			b.Jmp.Cases[i].Val = d.iv()
			b.Jmp.Cases[i].Blk = blockRef()
		}
	}
}

func (d *dec) decInstr(blockRef func() *Block) Instr {
	in := Instr{Op: Op(d.uv()), Cls: Cls(d.u8()), To: d.ref()}
	in.Args = make([]Ref, int(d.uv()))
	for i := range in.Args {
		in.Args[i] = d.ref()
	}
	in.Cmp = Cmp(d.u8())
	in.Aux = d.iv()
	in.Unroll = int32(d.iv())
	in.CallConv = CallConvention(d.u8())
	in.CallConvSet = d.boolean()
	in.Amode = int32(d.iv())
	if n := int(d.uv()); n > 0 {
		in.AggArgs = make([]*AggType, n)
		for i := range in.AggArgs {
			in.AggArgs[i] = d.typeRef()
		}
	}
	in.ArgGroups = d.valueGroups()
	if n := int(d.uv()); n > 0 {
		in.Defs = make([]Ref, n)
		for i := range in.Defs {
			in.Defs[i] = d.ref()
		}
	}
	in.RetAgg = d.typeRef()
	in.RetValues = d.boolean()
	in.StackResult = d.ref()
	in.StackResultOffset = d.iv()
	in.Pos = d.srcPos()
	in.Tail = d.boolean()
	in.Volatile = d.boolean()
	in.ClosureCall = d.boolean()
	in.ClosureContext = d.ref()
	in.Asm = d.asmOp()
	in.Intrin = d.intrinOp()
	in.Inl = d.inlineSite()
	in.Blk = blockRef()
	return in
}

func (d *dec) intrinOp() *IntrinOp {
	if !d.boolean() {
		return nil
	}
	return &IntrinOp{Name: d.str()}
}

func (d *dec) asmOp() *AsmOp {
	if !d.boolean() {
		return nil
	}
	a := &AsmOp{Template: d.str(), ExactClobbers: d.boolean()}
	if n := int(d.uv()); n > 0 {
		a.Ops = make([]AsmOperandKind, n)
		for i := range a.Ops {
			a.Ops[i] = AsmOperandKind(d.u8())
		}
	}
	if n := int(d.uv()); n > 0 {
		a.Regs = make([]string, n)
		for i := range a.Regs {
			a.Regs[i] = d.str()
		}
	}
	if n := int(d.uv()); n > 0 {
		a.Clobbers = make([]string, n)
		for i := range a.Clobbers {
			a.Clobbers[i] = d.str()
		}
	}
	return a
}

// inlineSite rebuilds the call-site chain, which was written innermost first.
// inlineSite decodes an inline-provenance chain, interning it so that two
// instructions from the same inline come back sharing one *InlineSite, as they
// shared one before the round trip.
//
// Interning is not a memory optimisation. Consumers group by pointer identity --
// arm64's buildInlineTree closes and opens a DWARF inlined-subroutine context on
// `stack[k].site == chain[k]` -- so a decode that gives each instruction its own
// chain turns one contiguous inlined region into one region per instruction.
// Measured on the small program, decoding every function and re-emitting:
// .debug_info goes from 3.4 MB to 10.0 MB and .rela.debug_info from 3.0 MB to
// 9.3 MB, for identical code. Two chains that are equal by value describe the
// same callee inlined at the same call site under the same parent, which is the
// same inline, so sharing is what they meant.
func (d *dec) inlineSite() *InlineSite {
	index := int(d.uv())
	if index == 0 || index > len(d.sites) {
		return nil
	}
	return d.sites[index-1]
}

// decInlineSites reads the function's site table. A parent is always numbered
// before the child that names it, so a single forward pass resolves every link.
func (d *dec) decInlineSites() []*InlineSite {
	n := int(d.uv())
	if n == 0 {
		return nil
	}
	sites := make([]*InlineSite, n)
	for i := range sites {
		site := &InlineSite{Callee: d.str(), Call: d.srcPos()}
		if parent := int(d.uv()); parent > 0 && parent <= i {
			site.Parent = sites[parent-1]
		}
		sites[i] = site
	}
	return sites
}
