package obj

import "sort"

// DWARF debug info. SetDWARF builds valid DWARF v4
// .debug_abbrev/.debug_info/.debug_line sections: a compilation unit with
// base-type and subprogram DIEs (functions with return types and parameters)
// and a line-number program mapping machine-code offsets back to source (file,
// line, column). A debugger can then show function names, set breakpoints by
// name, and step by source line — with no external assembler. Addresses are
// supplied via relocations against symbols, so the emitter need not know final
// addresses.

// A LineRow maps a byte offset within .text to a source position. File is a
// 1-based index into the files slice passed to SetDWARF.
type LineRow struct {
	TextOff uint64
	File    int
	Line    int
	Col     int
}

// DwarfType is a base (scalar) type referenced by a function's return value or a
// parameter. Encoding is a DW_ATE_* value.
type DwarfType struct {
	Name     string
	Encoding byte
	Size     int
}

// DwarfParam is a named formal parameter of a function.
type DwarfParam struct {
	Name string
	Type DwarfType
	Loc  *VarLoc // where the value lives, or nil if unknown
}

// VarLoc is a value's location over a PC range: the DWARF expression Expr is
// valid for code addresses in [Lo, Hi) (offsets within the enclosing function's
// compilation unit). Build Expr with LocReg or LocFrameBase.
type VarLoc struct {
	Lo, Hi uint64
	Expr   []byte
}

// LocReg builds a location expression naming a value that lives in DWARF
// register dwReg (DW_OP_reg / DW_OP_regx).
func LocReg(dwReg uint32) []byte {
	if dwReg <= 31 {
		return []byte{byte(0x50 + dwReg)} // DW_OP_reg0 + n
	}
	d := &dbuf{}
	d.u8(0x90) // DW_OP_regx
	d.uleb(uint64(dwReg))
	return d.b
}

// LocFrameBase builds a location expression for a value at the given byte offset
// from the frame base (DW_OP_fbreg), which the subprogram's DW_AT_frame_base
// resolves to the frame pointer.
func LocFrameBase(offset int64) []byte {
	d := &dbuf{}
	d.u8(0x91) // DW_OP_fbreg
	d.sleb(offset)
	return d.b
}

// DwarfFunc describes a function for a DW_TAG_subprogram DIE.
type DwarfFunc struct {
	Name     string
	Sym      string // symbol the low_pc address is relocated against
	Size     uint64 // byte size of the function's code
	DeclFile int    // 1-based file index, or 0 if unknown
	DeclLine int
	HasRet   bool
	RetType  DwarfType
	Params   []DwarfParam
	External bool
	Inlines  []InlineRange // inlined-callee regions within this function
}

// InlineRange is a contiguous run of code inlined from Callee, called at
// CallFile:CallLine. Lo/Hi are byte offsets within the enclosing function.
// Children are inlines nested inside this one.
type InlineRange struct {
	Callee   string
	CallFile int
	CallLine int
	Lo, Hi   uint64
	Children []InlineRange
}

// DWARF tag/attribute/form/opcode constants used below.
const (
	dwTagCompileUnit       = 0x11
	dwTagBaseType          = 0x24
	dwTagSubprogram        = 0x2e
	dwTagFormalParameter   = 0x05
	dwTagInlinedSubroutine = 0x1d
	dwChildrenNo           = 0x00
	dwChildrenYes          = 0x01

	dwAtLocation       = 0x02
	dwAtName           = 0x03
	dwAtByteSize       = 0x0b
	dwAtInline         = 0x20
	dwAtAbstractOrigin = 0x31
	dwAtEncoding       = 0x3e
	dwAtStmtList       = 0x10
	dwAtLowPC          = 0x11
	dwAtHighPC         = 0x12
	dwAtLanguage       = 0x13
	dwAtCompDir        = 0x1b
	dwAtProducer       = 0x25
	dwAtDeclFile       = 0x3a
	dwAtDeclLine       = 0x3b
	dwAtExternal       = 0x3f
	dwAtFrameBase      = 0x40
	dwAtType           = 0x49
	dwAtCallFile       = 0x58
	dwAtCallLine       = 0x59

	dwFormAddr      = 0x01
	dwFormData2     = 0x05
	dwFormData8     = 0x07
	dwFormString    = 0x08
	dwFormData1     = 0x0b
	dwFormFlag      = 0x0c
	dwFormRef4      = 0x13
	dwFormSecOffset = 0x17
	dwFormExprloc   = 0x18

	dwLangC      = 0x0002
	dwOpReg29    = 0x6d // DW_OP_reg0 (0x50) + 29: the frame pointer
	dwInlInlined = 0x01

	// Abbreviation codes.
	abCompileUnit = 1
	abBaseType    = 2
	abSubprogram  = 3 // has a return type
	abSubprogVoid = 4 // no return type
	abFormalParam = 5
	abAbstract    = 6 // abstract instance of an inlined function
	abInlinedSub  = 7 // an inlined_subroutine instance

	// Standard line opcodes.
	dwLnsCopy        = 0x01
	dwLnsAdvancePC   = 0x02
	dwLnsAdvanceLine = 0x03
	dwLnsSetFile     = 0x04
	dwLnsSetColumn   = 0x05

	// Extended line opcodes.
	dwLneEndSequence = 0x01
	dwLneSetAddress  = 0x02

	lineBase   = -5
	lineRange  = 14
	opcodeBase = 13
)

// SetDWARF populates the DWARF debug sections: a compilation unit with base-type
// and subprogram DIEs (functions, their return types, and parameters) plus a
// line-number program. anchorSym is the symbol text addresses are measured from
// (the first function, at .text offset 0); relocAbs64 is the architecture's
// 64-bit absolute relocation type. rows must be sorted by TextOff.
func (o *Object) SetDWARF(files []string, rows []LineRow, funcs []DwarfFunc, textSize uint64, anchorSym, producer, compDir, unitName string, relocAbs64 uint32) {
	o.DebugAbbrev = buildAbbrev()

	line, setAddrOff := buildLineProgram(files, rows, textSize)
	o.DebugLine = line
	o.DebugLineRelocs = []Reloc{{Offset: uint64(setAddrOff), Sym: anchorSym, Type: relocAbs64, Addend: 0}}

	loc, locOff := buildLoc(funcs)
	o.DebugLoc = loc

	info, relocs := buildInfo(funcs, locOff, textSize, anchorSym, producer, compDir, unitName, relocAbs64)
	o.DebugInfo = info
	o.DebugInfoRelocs = relocs
}

// buildLoc builds the .debug_loc section: one location list per parameter that
// has a known location. Offset 0 is a reserved empty list, which parameters
// without a location point at. Returns the section bytes and a map from each
// VarLoc to its offset. Location-list address ranges are relative to the CU base
// (the anchor at .text offset 0), so no relocations are needed.
func buildLoc(funcs []DwarfFunc) ([]byte, map[*VarLoc]uint32) {
	b := &dbuf{}
	// A parameter with no location points at offset 0, which must then be an
	// empty list. Reserve it only when some parameter actually needs it, so no
	// unreferenced hole is left when every parameter has a location.
	for _, f := range funcs {
		for _, p := range f.Params {
			if p.Loc == nil {
				b.u64(0)
				b.u64(0) // offset 0: the empty list (end-of-list marker)
				break
			}
		}
		if len(b.b) > 0 {
			break
		}
	}
	off := map[*VarLoc]uint32{}
	for _, f := range funcs {
		for _, p := range f.Params {
			if p.Loc == nil || off[p.Loc] != 0 {
				continue
			}
			off[p.Loc] = uint32(len(b.b))
			b.u64(p.Loc.Lo)
			b.u64(p.Loc.Hi)
			b.u16(uint16(len(p.Loc.Expr)))
			b.bytes(p.Loc.Expr)
			b.u64(0)
			b.u64(0) // end of this list
		}
	}
	return b.b, off
}

func buildAbbrev() []byte {
	b := &dbuf{}
	decl := func(code, tag, children uint64, attrs ...uint64) {
		b.uleb(code)
		b.uleb(tag)
		b.u8(byte(children))
		for i := 0; i < len(attrs); i += 2 {
			b.uleb(attrs[i])
			b.uleb(attrs[i+1])
		}
		b.uleb(0)
		b.uleb(0) // end of this abbreviation's attributes
	}
	decl(abCompileUnit, dwTagCompileUnit, dwChildrenYes,
		dwAtProducer, dwFormString, dwAtLanguage, dwFormData2, dwAtName, dwFormString,
		dwAtCompDir, dwFormString, dwAtLowPC, dwFormAddr, dwAtHighPC, dwFormData8,
		dwAtStmtList, dwFormSecOffset)
	decl(abBaseType, dwTagBaseType, dwChildrenNo,
		dwAtName, dwFormString, dwAtEncoding, dwFormData1, dwAtByteSize, dwFormData1)
	decl(abSubprogram, dwTagSubprogram, dwChildrenYes,
		dwAtExternal, dwFormFlag, dwAtName, dwFormString, dwAtDeclFile, dwFormData1,
		dwAtDeclLine, dwFormData2, dwAtLowPC, dwFormAddr, dwAtHighPC, dwFormData8,
		dwAtFrameBase, dwFormExprloc, dwAtType, dwFormRef4)
	decl(abSubprogVoid, dwTagSubprogram, dwChildrenYes,
		dwAtExternal, dwFormFlag, dwAtName, dwFormString, dwAtDeclFile, dwFormData1,
		dwAtDeclLine, dwFormData2, dwAtLowPC, dwFormAddr, dwAtHighPC, dwFormData8,
		dwAtFrameBase, dwFormExprloc)
	decl(abFormalParam, dwTagFormalParameter, dwChildrenNo,
		dwAtLocation, dwFormSecOffset, dwAtName, dwFormString, dwAtType, dwFormRef4)
	decl(abAbstract, dwTagSubprogram, dwChildrenNo,
		dwAtInline, dwFormData1, dwAtName, dwFormString)
	decl(abInlinedSub, dwTagInlinedSubroutine, dwChildrenYes,
		dwAtAbstractOrigin, dwFormRef4, dwAtCallFile, dwFormData1,
		dwAtCallLine, dwFormData2, dwAtLowPC, dwFormAddr, dwAtHighPC, dwFormData8)
	b.u8(0) // end of abbrev table
	return b.b
}

// buildInfo builds the compilation-unit DIE tree (base types + subprograms) and
// returns the section bytes and the relocations for every low_pc address.
func buildInfo(funcs []DwarfFunc, locOff map[*VarLoc]uint32, textSize uint64, anchorSym, producer, compDir, unitName string, relocAbs64 uint32) ([]byte, []Reloc) {
	b := &dbuf{}
	var relocs []Reloc
	addrOff := func(sym string, addend int64) {
		relocs = append(relocs, Reloc{Offset: uint64(len(b.b)), Sym: sym, Type: relocAbs64, Addend: addend})
		b.u64(0)
	}

	lenPos := len(b.b)
	b.u32(0) // unit_length placeholder
	b.u16(4) // version
	b.u32(0) // debug_abbrev_offset (single CU: abbrev starts at 0)
	b.u8(8)  // address_size

	// Compilation-unit DIE.
	b.uleb(abCompileUnit)
	b.cstr(producer)
	b.u16(dwLangC)
	b.cstr(unitName)
	b.cstr(compDir)
	addrOff(anchorSym, 0) // low_pc
	b.u64(textSize)       // high_pc
	b.u32(0)              // stmt_list

	// Base-type DIEs (children of the CU), deduplicated by name; record each
	// one's offset from the CU start so parameters/return types can ref4 it.
	typeOff := map[string]uint32{}
	emitType := func(t DwarfType) {
		if t.Name == "" || typeOff[t.Name] != 0 {
			return
		}
		typeOff[t.Name] = uint32(len(b.b))
		b.uleb(abBaseType)
		b.cstr(t.Name)
		b.u8(t.Encoding)
		b.u8(byte(t.Size))
	}
	for _, f := range funcs {
		if f.HasRet {
			emitType(f.RetType)
		}
		for _, p := range f.Params {
			emitType(p.Type)
		}
	}

	// Abstract-instance DIEs, one per distinct inlined callee, referenced by the
	// inlined_subroutine DIEs below. Emitted in sorted order for determinism.
	abstractOff := map[string]uint32{}
	for _, name := range inlinedCallees(funcs) {
		abstractOff[name] = uint32(len(b.b))
		b.uleb(abAbstract)
		b.u8(dwInlInlined)
		b.cstr(name)
	}

	// Emit an inlined_subroutine DIE (and its nested children) for one region.
	var emitInline func(r InlineRange, funcSym string)
	emitInline = func(r InlineRange, funcSym string) {
		b.uleb(abInlinedSub)
		b.u32(abstractOff[r.Callee]) // abstract_origin
		b.u8(byte(r.CallFile))
		b.u16(uint16(r.CallLine))
		addrOff(funcSym, int64(r.Lo)) // low_pc
		b.u64(r.Hi - r.Lo)            // high_pc
		for _, c := range r.Children {
			emitInline(c, funcSym)
		}
		b.u8(0) // end of this inlined_subroutine's children
	}

	// Subprogram DIEs.
	for _, f := range funcs {
		if f.HasRet {
			b.uleb(abSubprogram)
		} else {
			b.uleb(abSubprogVoid)
		}
		b.u8(boolByte(f.External))
		b.cstr(f.Name)
		b.u8(byte(f.DeclFile))
		b.u16(uint16(f.DeclLine))
		addrOff(f.Sym, 0) // low_pc
		b.u64(f.Size)     // high_pc
		b.uleb(1)         // frame_base exprloc: length 1...
		b.u8(dwOpReg29)
		if f.HasRet {
			b.u32(typeOff[f.RetType.Name])
		}
		for _, p := range f.Params {
			b.uleb(abFormalParam)
			b.u32(locOff[p.Loc]) // DW_AT_location: 0 (empty list) when unknown
			b.cstr(p.Name)
			b.u32(typeOff[p.Type.Name])
		}
		for _, r := range f.Inlines {
			emitInline(r, f.Sym)
		}
		b.u8(0) // end of this subprogram's children
	}

	b.u8(0) // end of the CU's children
	b.patchU32(lenPos, uint32(len(b.b)-lenPos-4))
	return b.b, relocs
}

// inlinedCallees returns the sorted set of callee names referenced by any inline
// region (recursively) across all functions.
func inlinedCallees(funcs []DwarfFunc) []string {
	seen := map[string]bool{}
	var walk func(rs []InlineRange)
	walk = func(rs []InlineRange) {
		for _, r := range rs {
			seen[r.Callee] = true
			walk(r.Children)
		}
	}
	for _, f := range funcs {
		walk(f.Inlines)
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

// buildLineProgram builds the .debug_line section and returns the byte offset of
// the set_address operand (to be relocated to the anchor symbol).
func buildLineProgram(files []string, rows []LineRow, textSize uint64) ([]byte, int) {
	b := &dbuf{}
	lenPos := len(b.b)
	b.u32(0) // unit_length placeholder
	b.u16(4) // version
	hlenPos := len(b.b)
	b.u32(0) // header_length placeholder
	progStart := len(b.b)
	b.u8(1)         // minimum_instruction_length (advance_pc is in bytes)
	b.u8(1)         // maximum_operations_per_instruction
	b.u8(1)         // default_is_stmt
	b.i8(lineBase)  // line_base
	b.u8(lineRange) // line_range
	b.u8(opcodeBase)
	// standard_opcode_lengths for opcodes 1..12.
	b.bytes([]byte{0, 1, 1, 1, 1, 0, 0, 0, 1, 0, 0, 1})
	b.u8(0) // include_directories: empty, terminating null
	for _, f := range files {
		b.cstr(f)
		b.uleb(0) // dir index
		b.uleb(0) // mtime
		b.uleb(0) // length
	}
	b.u8(0) // file_names terminator
	_ = progStart
	b.patchU32(hlenPos, uint32(len(b.b)-hlenPos-4))

	// Program. State machine starts at address 0, file 1, line 1, column 0.
	b.u8(0)
	b.uleb(9)
	b.u8(dwLneSetAddress)
	setAddrOff := len(b.b)
	b.u64(0) // address, relocated to the anchor symbol

	addr, file, line, col := uint64(0), 1, 1, 0
	for _, r := range rows {
		if r.File != file {
			b.u8(dwLnsSetFile)
			b.uleb(uint64(r.File))
			file = r.File
		}
		if r.Col != col {
			b.u8(dwLnsSetColumn)
			b.uleb(uint64(r.Col))
			col = r.Col
		}
		if r.TextOff != addr {
			b.u8(dwLnsAdvancePC)
			b.uleb(r.TextOff - addr)
			addr = r.TextOff
		}
		if r.Line != line {
			b.u8(dwLnsAdvanceLine)
			b.sleb(int64(r.Line - line))
			line = r.Line
		}
		b.u8(dwLnsCopy)
	}
	// Advance to the end of text and close the sequence.
	if textSize > addr {
		b.u8(dwLnsAdvancePC)
		b.uleb(textSize - addr)
	}
	b.u8(0)
	b.uleb(1)
	b.u8(dwLneEndSequence)

	b.patchU32(lenPos, uint32(len(b.b)-lenPos-4))
	return b.b, setAddrOff
}

// --- little DWARF byte buffer ----------------------------------------------

type dbuf struct{ b []byte }

func (d *dbuf) u8(v byte)      { d.b = append(d.b, v) }
func (d *dbuf) i8(v int8)      { d.b = append(d.b, byte(v)) }
func (d *dbuf) u16(v uint16)   { d.b = append(d.b, byte(v), byte(v>>8)) }
func (d *dbuf) u32(v uint32)   { d.b = append(d.b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24)) }
func (d *dbuf) bytes(p []byte) { d.b = append(d.b, p...) }

func (d *dbuf) u64(v uint64) {
	d.b = append(d.b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
}

func (d *dbuf) cstr(s string) { d.b = append(d.b, s...); d.b = append(d.b, 0) }

func (d *dbuf) patchU32(pos int, v uint32) {
	d.b[pos] = byte(v)
	d.b[pos+1] = byte(v >> 8)
	d.b[pos+2] = byte(v >> 16)
	d.b[pos+3] = byte(v >> 24)
}

func (d *dbuf) uleb(v uint64) {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		d.b = append(d.b, c)
		if v == 0 {
			return
		}
	}
}

func (d *dbuf) sleb(v int64) {
	for {
		c := byte(v & 0x7f)
		v >>= 7 // arithmetic shift
		last := (v == 0 && c&0x40 == 0) || (v == -1 && c&0x40 != 0)
		if !last {
			c |= 0x80
		}
		d.b = append(d.b, c)
		if last {
			return
		}
	}
}
