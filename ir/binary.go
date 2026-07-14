package ir

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
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
	binVersion = 1
)

// MarshalBinary encodes the module to cg12's binary unit format.
func (m *Module) MarshalBinary() ([]byte, error) {
	types, typeIdx := collectTypes(m)
	e := &enc{typeIdx: typeIdx}
	e.buf = append(e.buf, binMagic...)
	e.u8(binVersion)

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

// DecodeModule decodes a module from cg12's binary unit format.
func DecodeModule(data []byte) (*Module, error) {
	if len(data) < len(binMagic)+1 || string(data[:len(binMagic)]) != binMagic {
		return nil, errors.New("ir: not a cg12 unit (bad magic)")
	}
	d := &dec{buf: data, pos: len(binMagic)}
	if v := d.u8(); v != binVersion {
		return nil, fmt.Errorf("ir: unit format version %d, want %d", v, binVersion)
	}

	m := NewModule()
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
			}
		}
	}
	return list, idx
}

// --- encoder --------------------------------------------------------------

type enc struct {
	buf     []byte
	typeIdx map[*AggType]int
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
}

func (e *enc) encData(d *Data) {
	e.str(d.Name)
	e.linkage(d.Linkage)
	e.iv(int64(d.Align))
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
	e.boolean(f.Variadic)

	e.uv(uint64(len(f.Temps)))
	for _, t := range f.Temps {
		e.encTemp(t)
	}
	e.uv(uint64(len(f.Consts)))
	for _, c := range f.Consts {
		e.encConst(c)
	}
	e.uv(uint64(len(f.Params)))
	for _, p := range f.Params {
		e.uv(uint64(p.ID)) // Params are Temps; ID is the index into Temps
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
	e.iv(int64(b.ID))
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
	e.uv(uint64(len(in.AggArgs)))
	for _, t := range in.AggArgs {
		e.typeRef(t)
	}
	e.uv(uint64(len(in.Defs)))
	for _, r := range in.Defs {
		e.ref(r)
	}
	e.typeRef(in.RetAgg)
	e.srcPos(in.Pos)
	e.boolean(in.Tail)
	blockRef(in.Blk) // OBlockAddr target (nil for every other op)
}

// --- decoder --------------------------------------------------------------

type dec struct {
	buf   []byte
	pos   int
	err   error
	types []*AggType
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
	return Field{Sub: SubCls(d.u8()), Type: d.typeRef(), Count: int(d.iv())}
}

func (d *dec) decData() *Data {
	dt := &Data{Name: d.str(), Linkage: d.linkage(), Align: int(d.iv())}
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
	f.Variadic = d.boolean()

	f.Temps = make([]*Temp, int(d.uv()))
	for i := range f.Temps {
		f.Temps[i] = d.decTemp(i)
	}
	f.Consts = make([]Const, int(d.uv()))
	for i := range f.Consts {
		f.Consts[i] = d.decConst()
	}
	f.Params = make([]*Temp, int(d.uv()))
	for i := range f.Params {
		if id := int(d.uv()); id < len(f.Temps) {
			f.Params[i] = f.Temps[id]
		} else {
			d.fail("param temp index")
		}
	}

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
	return &Temp{
		ID:    id,
		Name:  d.str(),
		Cls:   Cls(d.u8()),
		Slot:  int(d.iv()),
		Reg:   int(d.iv()),
		Fixed: d.boolean(),
		Agg:   d.typeRef(),
	}
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
	b.ID = int(d.iv())
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
	if n := int(d.uv()); n > 0 {
		in.AggArgs = make([]*AggType, n)
		for i := range in.AggArgs {
			in.AggArgs[i] = d.typeRef()
		}
	}
	if n := int(d.uv()); n > 0 {
		in.Defs = make([]Ref, n)
		for i := range in.Defs {
			in.Defs[i] = d.ref()
		}
	}
	in.RetAgg = d.typeRef()
	in.Pos = d.srcPos()
	in.Tail = d.boolean()
	in.Blk = blockRef()
	return in
}
