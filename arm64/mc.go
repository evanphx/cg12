package arm64

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"runtime"
	"sort"
	"strings"

	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/internal/gometa"
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

// CompileObject emits the IR portion of m as an ELF relocatable object with
// default options. Call CompileObjectAndAssembly when m carries Plan 9 assembly
// that must be linked alongside the object.
func CompileObject(m *ir.Module) ([]byte, error) {
	return CompileObjectWith(m, Options{})
}

// CompileObjectWith emits an ELF relocatable object for m, applying opts (such as
// a GC strategy).
func CompileObjectWith(m *ir.Module, opts Options) ([]byte, error) {
	bundle, err := prepareAssembly(m)
	if err != nil {
		return nil, err
	}
	return compileObjectWithBundle(m, opts, bundle)
}

// CompileObjectAndAssembly emits the cg12 ELF object and GNU assembly parts of
// a module after parsing its Plan 9 assembly sources once.
func CompileObjectAndAssembly(m *ir.Module) ([]byte, string, error) {
	bundle, err := prepareAssembly(m)
	if err != nil {
		return nil, "", err
	}
	data, err := compileObjectWithBundle(m, Options{}, bundle)
	if err != nil {
		return nil, "", err
	}
	return data, bundle.source, nil
}

// CompileToObjectAndAssembly builds the in-memory relocatable object for m and
// returns the GNU assembly translated from its Plan 9 sources alongside it.
//
// It is CompileObjectAndAssembly for a caller that has to edit the object before
// it is serialized -- the driver split writes the prebuilt runtime module's
// moduledata.next, which names a symbol the program module has not been compiled
// yet to define.
func CompileToObjectAndAssembly(m *ir.Module) (*obj.Object, string, error) {
	bundle, err := prepareAssembly(m)
	if err != nil {
		return nil, "", err
	}
	object, err := compileToObjectWithBundle(m, Options{}, bundle)
	if err != nil {
		return nil, "", err
	}
	return object, bundle.source, nil
}

// WriteObjectAndAssembly emits the cg12 ELF object directly to w and returns
// the GNU assembly translated from the module's Plan 9 sources. Streaming the
// ELF object avoids allocating another complete copy of large program images.
func WriteObjectAndAssembly(w io.Writer, m *ir.Module) (string, error) {
	bundle, err := prepareAssembly(m)
	if err != nil {
		return "", err
	}
	object, err := compileToObjectWithBundle(m, Options{}, bundle)
	if err != nil {
		return "", err
	}
	if err := object.WriteELF(w); err != nil {
		return "", err
	}
	return bundle.source, nil
}

func compileObjectWithBundle(m *ir.Module, opts Options, bundle assemblyBundle) ([]byte, error) {
	o, err := compileToObjectWithBundle(m, opts, bundle)
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

func CompileToObjectWith(m *ir.Module, opts Options) (*obj.Object, error) {
	bundle, err := prepareAssembly(m)
	if err != nil {
		return nil, err
	}
	return compileToObjectWithBundle(m, opts, bundle)
}

// addNeutralData emits a platform (non-Go-runtime) module's data definitions
// with the ordinary data layout. It is the neutral counterpart to finishGoModule.
func addNeutralData(o *obj.Object, dataDefinitions []*ir.Data, bundle assemblyBundle) error {
	for _, d := range dataDefinitions {
		if bundle.definitions[sanitize(d.Name)] && !semanticDataDefinition(bundle.loweredData, d) {
			continue
		}
		if err := addData(o, d); err != nil {
			return fmt.Errorf("data %s: %w", d.Name, err)
		}
	}
	return nil
}

func compileToObjectWithBundle(m *ir.Module, opts Options, bundle assemblyBundle) (*obj.Object, error) {
	o := &obj.Object{Machine: obj.EM_AARCH64}
	assemblyReferences := bundle.references
	goRuntime := moduleUsesGoRuntime(m)
	var rows []obj.LineRow
	var dfuncs []obj.DwarfFunc
	var smFuncs []stackMapFunc
	var goFunctions []goFunctionInfo
	anchor := ""
	releaseFunctionIR := len(m.Funcs) > 2048
	functions := make([]*ir.Func, 0, len(m.Funcs)+len(bundle.lowered))
	functions = append(functions, m.Funcs...)
	functions = append(functions, bundle.lowered...)
	// Every call is lowered and emitted against its *callee's* convention, so the
	// index of what this object defines is built once, before the first function
	// is touched, and handed to both the lowering and the emitter. Building it per
	// function would be quadratic, and letting the two passes each decide would
	// let them disagree about a single call -- a worse failure than getting the
	// convention wrong, because half the sequence would use each ABI.
	conventions := newCalleeConventions(functions)
	// Annotate managed functions with their stack pointer-word maps first. This is
	// a pure IR-to-IR pass -- it reads only frontend IR and writes
	// f.StackPointerWords, with no dependency on code emission or on anything the
	// emit loop computes -- so it runs once up front rather than inside the driver.
	for _, f := range functions {
		if f.UsesManagedFrame() {
			prepareGoABI(f)
		}
	}
	// Lowering, allocating and emitting one function reads only that function and
	// read-only module facts, so the functions are compiled concurrently. Merging
	// them into the object stays strictly in emission order, which is what keeps the
	// output byte-identical regardless of the worker count: every address, symbol and
	// relocation below is derived from the merge order, never from the order the
	// workers happen to finish in.
	err := compileFunctionsInOrder(functions, opts, conventions, bundle, goRuntime, func(functionIndex int, code *functionCode) {
		base := uint64(len(o.Text))
		if anchor == "" {
			anchor = code.name
		}
		o.Text = append(o.Text, code.code...)
		goFunctions = append(goFunctions, code.goFunction)
		o.Syms = append(o.Syms, obj.Sym{
			Name: code.name, Section: obj.SecText, Value: base,
			Size: uint64(len(code.code)), Global: code.export || assemblyReferences[code.name], Func: true,
		})
		// Local symbols at address-taken blocks (&&label used in static data).
		for _, bs := range code.blockSyms {
			o.Syms = append(o.Syms, obj.Sym{
				Name: bs.name, Section: obj.SecText, Value: base + uint64(bs.off),
			})
		}
		for _, rl := range code.relocs {
			rl.Offset += base
			o.Relocs = append(o.Relocs, rl)
		}
		dfuncs = append(dfuncs, code.dwarf)
		for _, r := range code.rows {
			r.TextOff += base
			rows = append(rows, r)
		}
		if code.stackMap != nil {
			smFuncs = append(smFuncs, *code.stackMap)
		}
		if releaseFunctionIR && functionIndex < len(m.Funcs) {
			// Large source-compiled standard-library modules are one-shot inputs.
			// Every object, debug, and runtime-metadata detail for this function has
			// been copied above, so retaining its lowered IR only inflates the peak
			// while stack maps are encoded. Both slices have to let go: functions
			// holds the same pointers m.Funcs does.
			m.Funcs[functionIndex] = nil
			functions[functionIndex] = nil
		}
	})
	if err != nil {
		return nil, err
	}
	if releaseFunctionIR {
		m.Funcs = nil
		runtime.GC()
	}
	dataDefinitions := make([]*ir.Data, 0, len(m.Data)+len(bundle.loweredData))
	dataDefinitions = append(dataDefinitions, m.Data...)
	dataDefinitions = append(dataDefinitions, bundle.loweredData...)
	// The neutral emit loop above has produced the code and gathered per-function
	// facts; the module data layout and any runtime metadata are the frontend's to
	// finish. A Go runtime module lays its data out with the runtime's BSS/fixup
	// layout and assembles pclntab/moduledata from goFunctions; a platform module
	// emits ordinary data and no metadata.
	if goRuntime {
		if err := finishGoModule(o, m, goFunctions, dataDefinitions, bundle); err != nil {
			return nil, err
		}
	} else {
		if err := addNeutralData(o, dataDefinitions, bundle); err != nil {
			return nil, err
		}
	}
	if err := addAliases(o, m.Aliases); err != nil {
		return nil, err
	}
	// Emit DWARF when the module carries a source-file table.
	if len(m.Files) > 0 && anchor != "" {
		o.SetDWARF(m.Files, rows, dfuncs, uint64(len(o.Text)), anchor, "cg12", ".", m.Files[0], obj.R_AARCH64_ABS64, 0x6d) // DW_OP_reg29 (x29)
	}
	// Emit legacy cg12 GC stack maps when any safepoint carries a live root.
	// Go-runtime modules already carry the runtime's native stack-map metadata in
	// moduledata/pclntab, so emitting the legacy section there only duplicates
	// the data and can dominate memory for large standard-library binaries.
	if len(smFuncs) > 0 {
		setStackMap(o, smFuncs)
	}
	for index := range o.Syms {
		if assemblyReferences[o.Syms[index].Name] {
			o.Syms[index].Global = true
		}
	}
	return o, nil
}

func applyAssemblyCallConventions(function *ir.Func, conventions map[string]ir.CallConvention) {
	for _, block := range function.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if instruction.Op != ir.OCall || len(instruction.Args) == 0 || instruction.Args[0].Kind != ir.RefConst {
				continue
			}
			callee := function.Consts[instruction.Args[0].ID]
			if callee.Kind != ir.ConstSym {
				continue
			}
			convention, ok := conventions[sanitize(callee.Sym)]
			if !ok {
				continue
			}
			instruction.CallConv = convention
			instruction.CallConvSet = true
		}
	}
}

func semanticDataDefinition(definitions []*ir.Data, candidate *ir.Data) bool {
	for _, definition := range definitions {
		if definition == candidate {
			return true
		}
	}
	return false
}

func safepointsHaveRoots(safepoints []safepoint) bool {
	for _, safepoint := range safepoints {
		if len(safepoint.roots) > 0 {
			return true
		}
	}
	return false
}

func goDataPointerOffsets(data *ir.Data, base, size uint64) ([]uint64, error) {
	pointers := make(map[uint64]bool)
	for _, word := range data.PointerWords {
		if word < 0 || uint64(word)*8+8 > size {
			return nil, fmt.Errorf("pointer word %d lies outside %d-byte definition", word, size)
		}
		pointers[base+uint64(word)*8] = true
	}

	cursor := uint64(0)
	for _, item := range data.Items {
		switch {
		case item.Zero > 0:
			cursor += uint64(item.Zero)
		case item.Str != "":
			cursor += uint64(len(item.Str))
		case item.Sym != "" && item.RelativeTo != "":
			cursor += uint64(item.Sub.Size())
		case item.Sym != "":
			size := uint64(item.Sub.Size())
			if size != 8 {
				cursor += size
				continue
			}
			if cursor%8 != 0 {
				return nil, fmt.Errorf("pointer relocation at unaligned byte offset %d", cursor)
			}
			pointers[base+cursor] = true
			cursor += size
		case len(item.Flts) > 0:
			cursor += uint64(len(item.Flts) * item.Sub.Size())
		default:
			cursor += uint64(len(item.Ints) * item.Sub.Size())
		}
	}

	offsets := make([]uint64, 0, len(pointers))
	for offset := range pointers {
		offsets = append(offsets, offset)
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	return offsets, nil
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
	total := 0
	totalRoots := 0
	for _, function := range funcs {
		total += len(function.points)
		for _, safepoint := range function.points {
			totalRoots += len(safepoint.roots)
		}
	}
	encodedSize := 12 + total*12 + totalRoots*12
	b := make([]byte, 0, encodedSize)
	if additional := total; additional > 0 {
		relocations := make([]obj.Reloc, len(o.StackMapRelocs), len(o.StackMapRelocs)+additional)
		copy(relocations, o.StackMapRelocs)
		o.StackMapRelocs = relocations
	}
	put16 := func(v uint16) { b = append(b, byte(v), byte(v>>8)) }
	put32 := func(v uint32) { b = append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24)) }
	put64 := func(v uint64) {
		for i := 0; i < 8; i++ {
			b = append(b, byte(v>>(8*i)))
		}
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
	return addDataWithBSSAndFixups(o, d, true, nil)
}

// addGoData keeps Go globals in one contiguous data range. The runtime's
// moduledata and GC program describe that range precisely; splitting zeroed
// pointer globals into ELF BSS requires a separate gcbss program and bounds.
func addGoData(o *obj.Object, d *ir.Data) error {
	return addDataWithBSSAndFixups(o, d, false, nil)
}

func addDataWithBSS(o *obj.Object, d *ir.Data, allowBSS bool) error {
	return addDataWithBSSAndFixups(o, d, allowBSS, nil)
}

type relativeDataFixup struct {
	section    obj.SecKind
	offset     uint64
	target     string
	relativeTo string
	addend     int64
	size       int
}

func addDataWithBSSAndFixups(o *obj.Object, d *ir.Data, allowBSS bool, fixups *[]relativeDataFixup) error {
	// Nothing but zero fill: it needs no bytes in the file, only room at run time.
	// .bss follows .data in memory and .tbss follows .tdata in each thread's block,
	// so the symbol's value counts from the start of its own zero-filled region.
	if allowBSS && allZero(d) {
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
		case it.Sym != "" && it.RelativeTo != "":
			if fixups == nil {
				target, targetFound := gometa.ObjectSymbol(o, sanitize(it.Sym))
				relativeTo, baseFound := gometa.ObjectSymbol(o, sanitize(it.RelativeTo))
				if !targetFound {
					return fmt.Errorf("arm64: relative data reference %q is undefined", it.Sym)
				}
				if !baseFound {
					return fmt.Errorf("arm64: relative data base %q is undefined", it.RelativeTo)
				}
				if target.Section != relativeTo.Section {
					return fmt.Errorf("arm64: relative data reference %q and base %q are in different sections", it.Sym, it.RelativeTo)
				}
				value := int64(target.Value) - int64(relativeTo.Value) + it.Off
				*buf = appendInt(*buf, value, it.Sub.Size())
				continue
			}
			*fixups = append(*fixups, relativeDataFixup{
				section:    sec,
				offset:     uint64(len(*buf)),
				target:     sanitize(it.Sym),
				relativeTo: sanitize(it.RelativeTo),
				addend:     it.Off,
				size:       it.Sub.Size(),
			})
			*buf = append(*buf, make([]byte, it.Sub.Size())...)
		case it.Sym != "":
			// A pointer stored in data: emit an 8-byte slot and a .rela.data
			// relocation carrying the (symbol + offset) addend.
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
			relocationType := uint32(obj.R_AARCH64_ABS64)
			relocationSize := 8
			if it.Sub.Size() == 4 {
				relocationType = obj.R_AARCH64_ABS32
				relocationSize = 4
			}
			*list = append(*list, obj.Reloc{
				Offset: uint64(len(*buf)), Sym: sanitize(it.Sym),
				Type: relocationType, Addend: it.Off,
			})
			*buf = append(*buf, make([]byte, relocationSize)...)
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
	name := sanitize(d.Name)
	o.Syms = append(o.Syms, obj.Sym{
		Name: name, Section: sec, Value: base,
		Size: uint64(len(*buf)) - base, Global: d.Linkage.Export,
		TLS: sec == obj.SecTdata,
	})
	return nil
}

func resolveRelativeDataFixups(o *obj.Object, fixups []relativeDataFixup) error {
	for _, fixup := range fixups {
		target, targetFound := gometa.ObjectSymbol(o, fixup.target)
		if !targetFound {
			return fmt.Errorf("target %q is undefined", fixup.target)
		}
		relativeTo, baseFound := gometa.ObjectSymbol(o, fixup.relativeTo)
		if !baseFound {
			return fmt.Errorf("base %q is undefined", fixup.relativeTo)
		}
		if target.Section != relativeTo.Section {
			return fmt.Errorf("target %q and base %q are in different sections", fixup.target, fixup.relativeTo)
		}

		buffer, ok := objectSectionData(o, fixup.section)
		if !ok {
			return fmt.Errorf("unsupported destination section %d", fixup.section)
		}
		if fixup.offset+uint64(fixup.size) > uint64(len(buffer)) {
			return fmt.Errorf("destination for %q lies outside section", fixup.target)
		}
		value := int64(target.Value) - int64(relativeTo.Value) + fixup.addend
		encoded := appendInt(nil, value, fixup.size)
		copy(buffer[fixup.offset:], encoded)
	}
	return nil
}

func objectSectionData(o *obj.Object, section obj.SecKind) ([]byte, bool) {
	switch section {
	case obj.SecData:
		return o.Data, true
	case obj.SecRodata:
		return o.Rodata, true
	case obj.SecRelro:
		return o.Relro, true
	case obj.SecTdata:
		return o.Tdata, true
	default:
		return nil, false
	}
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
	pc      uint64
	startPC uint64
	roots   []rootLoc
}

// mc emits AArch64 machine code for one function.
type mc struct {
	f *ir.Func
	// conventions is the object-wide callee-convention index. The emitter must
	// resolve a call's ABI exactly as the lowering did -- the stack-argument
	// offsets, the stack link, and whether SP moves around the call all depend on
	// it -- so both read the same map rather than each deciding for itself.
	conventions calleeConventions
	alloc       *allocation
	gc          GCStrategy
	tlsModel    TLSModel
	prog        *a64.Program
	relocs      []obj.Reloc
	allocTmp    map[uint32]int // fixed stack allocation temporary -> frame offset
	// stackAllocTmp includes pointer-bearing allocas as well as ordinary fixed
	// allocas. It is used to turn a live alloca address into the pointer words
	// contained by that alloca at each GC safepoint.
	stackAllocTmp map[uint32]int

	frameLayout      // where everything lives in the frame
	frameless   bool // leaf function needing no frame: no prologue/epilogue, ret directly

	// tbz/tbnz reach only +/-32 KiB, so whether a single-bit test can fuse into one
	// depends on the byte distance to its target. A first emission pass with tbz
	// disabled records these real offsets; the second pass fuses a tbz only when the
	// pass-1 distance is in range. Enabling a tbz only ever replaces a two-word
	// tst;b.cond with a one-word tbz, so pass-1 distances are a strict upper bound --
	// a branch in range under pass 1 stays in range under pass 2. Both nil in pass 1.
	refBlockPC map[*ir.Block]int // pass-1 byte offset of each block's start
	refTermPC  map[*ir.Block]int // pass-1 byte offset of each block's terminator
	gotTermPC  map[*ir.Block]int // terminator offsets recorded during THIS pass (nil to skip)

	instrPC    map[*ir.Instr]uint64 // PC at the start of each instruction
	safepoints []safepoint
	frameStart int

	blockDone    bool
	useCount     []int                     // per-temp use count, for the fused compare-branch
	nextBlock    *ir.Block                 // block laid out after the current one, for fall-through elision
	preds        map[*ir.Block][]*ir.Block // predecessors, for cross-block flag reuse
	vaSeq        int
	atomicSeq    int
	currentBlock string
	currentOp    ir.Op
	rows         []obj.LineRow // source positions, keyed by func-relative byte offset
	lastPos      ir.SrcPos
	inl          []inlSample // inline-context changes, keyed by func-relative offset
	lastInl      *ir.InlineSite
	err          error
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

// prepareGoABI runs the architecture-neutral IR annotations a managed function
// needs before lowering. The pass itself lives in ir (both backends need it and
// neither owns it); this is the arm64 hook that names which of them run.
func prepareGoABI(function *ir.Func) {
	ir.InferStackPointerWords(function)
}

// goRegisterPointerMask reports which x0-x15 argument registers contain
// pointers at function entry. The stack-growth trampoline uses this to expose
// only actual pointers to the runtime stack copier while preserving every
// untyped register value in an unscanned save area.
func goRegisterPointerMask(function *ir.Func) uint16 {
	assigner := newArgAssigner(function.UsesGoInternalCallConvention())
	var mask uint16
	for _, parameter := range function.Params {
		if parameter.Agg != nil {
			if function.UsesGoInternalCallConvention() {
				parts, onStack, _ := assignGoAggregate(&assigner, parameter.Agg)
				if onStack {
					continue
				}
				for _, part := range parts {
					if part.pointer && part.reg >= X0 && part.reg <= X15 {
						mask |= 1 << uint(part.reg-X0)
					}
				}
				continue
			}

			class := classifyAgg(parameter.Agg)
			switch class.kind {
			case aggGP:
				assigner.assignGP(class.nregs, class.size)
			case aggHFA:
				assigner.assignHFA(class.nregs, class.size)
			default:
				location := assigner.assign(ir.ClsL)
				if !location.onStack && location.reg <= X7 {
					mask |= 1 << uint(location.reg-X0)
				}
			}
			continue
		}

		location := assigner.assign(parameter.Cls)
		if parameter.GCRef && !location.onStack && location.reg >= X0 && location.reg <= X15 {
			mask |= 1 << uint(location.reg-X0)
		}
	}
	return mask
}

func newEmitter(f *ir.Func, alloc *allocation, conventions calleeConventions, gc GCStrategy, tlsModel TLSModel) *mc {
	frameLayout := computeFrame(f, alloc, conventions)
	m := &mc{
		f: f, conventions: conventions, alloc: alloc, gc: gc, tlsModel: tlsModel,
		prog: a64.NewProgram(), instrPC: map[*ir.Instr]uint64{},
		frameLayout:   frameLayout,
		allocTmp:      make(map[uint32]int),
		stackAllocTmp: make(map[uint32]int),
	}
	m.frameless = framelessEligible(f, m.frameLayout, gc)
	m.useCount = countTempUses(f)
	m.preds = blockPreds(f)
	for instruction, offset := range frameLayout.allocOff {
		if instruction.To.Kind != ir.RefTemp {
			continue
		}
		m.stackAllocTmp[instruction.To.ID] = offset
		if !f.Temp(instruction.To).GCRef {
			m.allocTmp[instruction.To.ID] = offset
		}
	}
	m.inferFrameAddressOffsets()
	return m
}

// emitBody runs the prologue and every block in layout order, recording each
// block's terminator offset when gotTermPC is set. It leaves the program
// unresolved (branches unpatched); the caller resolves it with Bytes.
func (m *mc) emitBody() error {
	m.prologue()
	order := layoutBlocks(m.f)
	for i, b := range order {
		m.prog.Label(b.Name)
		if i+1 < len(order) {
			m.nextBlock = order[i+1]
		} else {
			m.nextBlock = nil
		}
		m.block(b)
	}
	return m.err
}

func emitMachine(f *ir.Func, alloc *allocation, conventions calleeConventions, gc GCStrategy, tlsModel TLSModel) (*machineCode, error) {
	// Pass 1: tbz disabled (refTermPC nil), to measure real block/terminator
	// offsets. See the refBlockPC/refTermPC comment for why these are an upper bound.
	m1 := newEmitter(f, alloc, conventions, gc, tlsModel)
	m1.gotTermPC = map[*ir.Block]int{}
	if err := m1.emitBody(); err != nil {
		return nil, err
	}
	blockPC := map[*ir.Block]int{}
	for _, b := range f.Blocks {
		if off, ok := m1.prog.LabelOffset(b.Name); ok {
			blockPC[b] = off
		}
	}

	// Pass 2: tbz enabled, each fusion decided from the pass-1 distance.
	m := newEmitter(f, alloc, conventions, gc, tlsModel)
	m.refBlockPC, m.refTermPC = blockPC, m1.gotTermPC
	if err := m.emitBody(); err != nil {
		return nil, err
	}
	code, err := m.prog.Bytes()
	if err != nil {
		if !isTbzRangeErr(err) {
			return nil, err
		}
		// A fused tbz slipped out of range despite the upper-bound argument (should
		// not happen). Re-emit with tbz disabled -- correctness over the fusion.
		m = newEmitter(f, alloc, conventions, gc, tlsModel)
		if err := m.emitBody(); err != nil {
			return nil, err
		}
		if code, err = m.prog.Bytes(); err != nil {
			return nil, err
		}
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

func (m *mc) inferFrameAddressOffsets() {
	type dependentAddress struct {
		result uint32
		addend int
	}
	type addressDependency struct {
		base   uint32
		result uint32
		addend int
	}

	definitionCounts := make(map[uint32]int)
	var dependencies []addressDependency
	for _, block := range m.f.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			// OReload restores the same logical value after a call; it is not an
			// independent address definition and must not prevent propagation from
			// the original fixed-frame address.
			if instruction.To.Kind == ir.RefTemp && instruction.Op != ir.OReload {
				definitionCounts[instruction.To.ID]++
			}
			base, addend, known := m.frameAddressDependency(instruction)
			if !known {
				continue
			}
			dependencies = append(dependencies, addressDependency{
				base:   base,
				result: instruction.To.ID,
				addend: addend,
			})
		}
	}

	// SSA destruction represents a phi with one copy to the same destination
	// on each incoming edge. Such a temporary is not a fixed frame address when
	// the edges supply different locals, so only propagate through values with a
	// single logical definition.
	dependents := make(map[uint32][]dependentAddress)
	for _, dependency := range dependencies {
		if definitionCounts[dependency.result] != 1 {
			continue
		}
		dependents[dependency.base] = append(dependents[dependency.base], dependentAddress{
			result: dependency.result,
			addend: dependency.addend,
		})
	}

	work := make([]uint32, 0, len(m.allocTmp))
	for temporary := range m.allocTmp {
		work = append(work, temporary)
	}
	for next := 0; next < len(work); next++ {
		base := work[next]
		baseOffset := m.allocTmp[base]
		for _, dependent := range dependents[base] {
			if _, alreadyKnown := m.allocTmp[dependent.result]; alreadyKnown {
				continue
			}
			m.allocTmp[dependent.result] = baseOffset + dependent.addend
			work = append(work, dependent.result)
		}
	}
}

func (m *mc) frameAddressDependency(instruction *ir.Instr) (uint32, int, bool) {
	if instruction.To.Kind != ir.RefTemp {
		return 0, 0, false
	}

	switch instruction.Op {
	case ir.OCopy:
		if len(instruction.Args) != 1 || instruction.Args[0].Kind != ir.RefTemp {
			return 0, 0, false
		}
		return instruction.Args[0].ID, 0, true

	case ir.OAdd:
		if len(instruction.Args) != 2 || instruction.Args[0].Kind != ir.RefTemp || instruction.Args[1].Kind != ir.RefConst {
			return 0, 0, false
		}
		constant := m.f.Consts[instruction.Args[1].ID]
		if constant.Kind != ir.ConstInt {
			return 0, 0, false
		}
		return instruction.Args[0].ID, int(constant.Int), true

	default:
		return 0, 0, false
	}
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
		message := fmt.Sprintf(format, a...)
		if m.currentBlock != "" {
			message = fmt.Sprintf("%s while emitting %s.%s", message, m.f.Name, m.currentBlock)
			if m.currentOp != 0 {
				message = fmt.Sprintf("%s %s", message, m.currentOp)
			}
		}
		m.err = fmt.Errorf("%s", message)
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

// framelessEligible reports whether f needs no stack frame at all: the AArch64
// ABI only forces the x29/x30 save when the function calls (which clobbers x30,
// the return address) or has something to store on the stack. A leaf whose frame
// is just the x29/x30 slot has neither, so the save is pure overhead -- x30 still
// holds the return address at `ret` (nothing called), and x29 is never allocated
// (it is excluded from intAllocOrder), so leaving it untouched keeps the caller's
// frame pointer intact. A stack-growth prologue needs the frame, so it disables
// this. Because the frame is exactly 16 bytes here there are no spills, allocas,
// callee-saved registers, or a variadic save area to address off x29.
func framelessEligible(f *ir.Func, lay frameLayout, gc GCStrategy) bool {
	if f.UsesManagedFrame() {
		return false
	}
	if lay.frame != 16 || lay.hasDynAlloc || lay.variadic {
		return false
	}
	if _, ok := gc.(PrologueEmitter); ok {
		return false
	}
	// An incoming stack argument lives above the frame and is read as [x29, #frame+off]
	// (mc.stackParam); it does not add to lay.frame, so frame==16 does not rule it out.
	// Without the frame those offsets are wrong, so a function that takes one keeps its
	// frame. Stack params are the arg-less OPar instructions at the start of the entry.
	if f.Start != nil {
		for i := range f.Start.Instrs {
			in := &f.Start.Instrs[i]
			if in.Op != ir.OPar {
				break // params are a prefix of the entry block
			}
			if len(in.Args) == 0 {
				return false
			}
		}
	}
	return isLeaf(f)
}

// isLeaf reports whether f emits no call. A call clobbers x30 (the return
// address), so a non-leaf must save it. Calls come from OCall (direct, indirect,
// or tail), a hidden call in an inline-asm template (OAsm), and -- crucially -- an
// OSafepoint, which a GC strategy may lower into a cooperative-poll call at emit
// time even though the IR marker itself is call-free. Treating the marker as a
// call keeps a polled function's frame, so the poll's bl finds x30 saved.
func isLeaf(f *ir.Func) bool {
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			switch b.Instrs[i].Op {
			case ir.OCall, ir.OAsm, ir.OSafepoint:
				return false
			}
		}
	}
	return true
}

func (m *mc) prologue() {
	if m.frameless {
		return
	}
	if m.f.UsesManagedFrame() {
		if !m.f.NoSplit {
			m.goStackPrologue()
		}
	}
	// A strategy may emit a stack-growth guard before the frame is set up; its
	// slow path branches back to this label to re-check after the stack grows.
	if pe, ok := m.gc.(PrologueEmitter); ok {
		m.prog.Label("__cg12_prologue")
		pe.EmitPrologue(&PrologueContext{mc: m, retry: "__cg12_prologue"})
	}
	m.allocFrame()
	if m.f.UsesManagedFrame() {
		m.frameStart = m.prog.Len() * 4
	}
	// Save the callee-saved registers, pairing two adjacent integer ones into a
	// single stp (they occupy adjacent 8-byte slots), as gcc does. Float saves and
	// a lone trailing register stay individual.
	cs := m.calleeSaved
	for i := 0; i < len(cs); {
		if i+1 < len(cs) && stpPairable(cs[i], cs[i+1], 16+i*8) {
			m.emit(a64.Stp(true, mreg(cs[i]), mreg(cs[i+1]), mcX29, 16+i*8, a64.SignedOffset))
			i += 2
			continue
		}
		m.spillStore(mreg(cs[i]), cs[i].IsFloat(), 16+i*8, 8)
		i++
	}
	m.zeroGoPointerSlots()
	if m.variadic {
		for i := 0; i < 8; i++ {
			m.emit(a64.StrImm(true, a64.Reg(i), mcX29, uint32(m.gpSaveOff+i*8)))
		}
		for i := 0; i < 8; i++ {
			m.emit(a64.StrFP(true, a64.Reg(i), mcX29, uint32(m.fpSaveOff+i*16)))
		}
	}
}

// stpPairable reports whether two adjacent callee-saved registers at frame offset
// off can be saved or restored with a single 64-bit stp/ldp: both must be integer
// (a float pair uses a distinct encoding) and off a multiple of 8 within the
// scaled 7-bit signed range.
func stpPairable(a, b Reg, off int) bool {
	return !a.IsFloat() && !b.IsFloat() && off%8 == 0 && off >= -512 && off <= 504
}

func (m *mc) zeroGoPointerSlots() {
	if !m.f.UsesManagedFrame() {
		return
	}
	for _, off := range goPointerFrameOffsets(m.f, m.allocOff, m.spillBase) {
		m.spillStore(a64.Reg(31), false, off, 8)
	}
}

func (m *mc) goStackPrologue() {
	const retry = "__cg12_go_prologue"
	const enough = "__cg12_go_stack_ok"

	stackGuardOffset := 16
	morestackSymbol := "runtime_morestack_noctxt"
	if m.f.SystemStack {
		stackGuardOffset = 24
		morestackSymbol = "runtime_morestackc"
	}

	m.prog.Label(retry)
	m.emit(a64.LdrImm(true, mcGP0, a64.Reg(28), uint32(stackGuardOffset)))
	m.emit(a64.AddImm(true, mcGP1, mcSP, 0))
	hi, lo := m.frame>>12, m.frame&0xfff
	if hi > 0 {
		m.emit(a64.SubImmLSL12(true, mcGP1, mcGP1, uint32(hi)))
	}
	if lo > 0 {
		m.emit(a64.SubImm(true, mcGP1, mcGP1, uint32(lo)))
	}
	m.emit(a64.CmpReg(true, mcGP1, mcGP0))
	m.prog.Bcond(a64.HI, enough)
	spills := goRegisterSpills(m.f)
	for _, spill := range spills {
		m.emitGoRegisterSpill(spill, false)
	}
	m.emit(a64.AddImm(true, a64.Reg(3), mcX30, 0))
	m.reloc(morestackSymbol, obj.R_AARCH64_CALL26)
	m.emit(a64.Bl(0))
	for _, spill := range spills {
		m.emitGoRegisterSpill(spill, true)
	}
	m.prog.B(retry)
	m.prog.Label(enough)
}

func (m *mc) emitGoRegisterSpill(spill goRegisterSpill, load bool) {
	base := mcSP
	offset := spill.offset
	if !spillImmFits(offset, spill.size) {
		m.movImm(mcGP1, int64(offset), true)
		m.emit(a64.AddReg(true, mcGP1, mcSP, mcGP1))
		base = mcGP1
		offset = 0
	}
	if load {
		m.ldFrom(mreg(spill.reg), spill.float, base, offset, spill.size)
		return
	}
	m.stTo(mreg(spill.reg), spill.float, base, offset, spill.size)
}

func (m *mc) allocFrame() {
	if !m.f.UsesManagedFrame() {
		if m.frame <= 504 {
			m.emit(a64.Stp(true, mcX29, mcX30, mcSP, -m.frame, a64.PreIndex))
		} else {
			m.adjustSP(true, m.frame)
			m.emit(a64.Stp(true, mcX29, mcX30, mcSP, 0, a64.SignedOffset))
		}
		m.emit(a64.AddImm(true, mcX29, mcSP, 0))
		return
	}
	m.adjustSP(true, m.frame)
	if m.outgoing == 0 {
		m.emit(a64.Stp(true, mcX29, mcX30, mcSP, 0, a64.SignedOffset))
		m.emit(a64.AddImm(true, mcX29, mcSP, 0))
		return
	}
	m.emit(a64.AddImm(true, mcGP1, mcSP, 0))
	outgoingHigh := m.outgoing >> 12
	outgoingLow := m.outgoing & 0xfff
	if outgoingHigh > 0 {
		m.emit(a64.AddImmLSL12(true, mcGP1, mcGP1, uint32(outgoingHigh)))
	}
	if outgoingLow > 0 {
		m.emit(a64.AddImm(true, mcGP1, mcGP1, uint32(outgoingLow)))
	}
	m.emit(a64.Stp(true, mcX29, mcX30, mcGP1, 0, a64.SignedOffset))
	m.emit(a64.AddImm(true, mcX29, mcGP1, 0))
}

func (m *mc) frameTeardown() {
	if !m.f.UsesManagedFrame() {
		(m.newSel()).
			frameTeardown(m.frame, m.hasDynAlloc, m.calleeSaved)
		return
	}
	for i, r := range m.calleeSaved {
		m.spillLoad(mreg(r), r.IsFloat(), 16+i*8, 8)
	}
	// Read the frame record before changing x29, then reconstruct the bottom of
	// this frame from FP. This also discards any dynamic allocation below SP.
	m.emit(a64.LdrImm(true, mcGP1, mcX29, 0))
	m.emit(a64.LdrImm(true, mcX30, mcX29, 8))
	m.emit(a64.AddImm(true, mcSP, mcX29, 0))
	m.adjustSP(true, m.outgoing)
	m.emit(a64.AddImm(true, mcX29, mcGP1, 0))
	m.adjustSP(false, m.frame)
}

func (m *mc) epilogue() {
	if m.frameless {
		m.emit(a64.Ret(mcX30)) // no frame to tear down
		return
	}
	m.frameTeardown()
	m.emit(a64.Ret(mcX30))
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

func (m *mc) frameTop() int {
	if m.f.UsesManagedFrame() {
		return m.frame - m.outgoing
	}
	return m.frame
}

func (m *mc) goPointerWords() []int {
	return goPointerWordIndexes(m.f, m.allocOff, m.spillBase)
}

func (m *mc) goLocalPointerWords() []int {
	return goLocalPointerWordIndexes(m.f, m.allocOff, m.spillBase)
}

func (m *mc) goStackMapPoints() []goStackMapPoint {
	points := make([]goStackMapPoint, 0, len(m.safepoints))
	for _, safepoint := range m.safepoints {
		seen := make(map[int]bool)
		for _, root := range safepoint.roots {
			if root.kind != rootFrame || root.val < 16 || root.val%8 != 0 {
				continue
			}
			seen[(int(root.val)-16)/8] = true
		}
		pointerWords := make([]int, 0, len(seen))
		for word := range seen {
			pointerWords = append(pointerWords, word)
		}
		sort.Ints(pointerWords)
		points = append(points, goStackMapPoint{
			PC:           int(safepoint.startPC),
			PointerWords: pointerWords,
		})
	}
	return points
}

func (m *mc) deferReturnOffset() (uint32, error) {
	registersDefer := false
	var deferReturn *ir.Instr
	for _, block := range m.f.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			symbol, ok := directCallSymbol(m.f, instruction)
			if !ok {
				continue
			}
			switch symbol {
			case "runtime_deferproc", "runtime_deferprocStack", "runtime_deferprocat":
				registersDefer = true
			case "runtime_deferreturn":
				if deferReturn == nil {
					deferReturn = instruction
				}
			}
		}
	}
	if deferReturn != nil {
		offset, emitted := m.instrPC[deferReturn]
		if !emitted || offset == 0 {
			return 0, fmt.Errorf("runtime.deferreturn has no valid emitted call-site PC")
		}
		return uint32(offset), nil
	}
	if registersDefer {
		return 0, fmt.Errorf("runtime defer registration has no defer-return continuation")
	}
	return 0, nil
}

func directCallSymbol(function *ir.Func, instruction *ir.Instr) (string, bool) {
	if instruction.Op != ir.OCall || len(instruction.Args) == 0 {
		return "", false
	}
	callee := instruction.Args[0]
	if callee.Kind != ir.RefConst {
		return "", false
	}
	constant := function.Consts[callee.ID]
	if constant.Kind != ir.ConstSym {
		return "", false
	}
	return sanitize(constant.Sym), true
}

func goPointerWordIndexes(function *ir.Func, allocations map[*ir.Instr]int, spillBase int) []int {
	seen := make(map[int]bool)
	for _, frameOffset := range goPointerFrameOffsets(function, allocations, spillBase) {
		word := (frameOffset - 16) / 8
		if frameOffset >= 16 {
			seen[word] = true
		}
	}
	words := make([]int, 0, len(seen))
	for word := range seen {
		words = append(words, word)
	}
	sort.Ints(words)
	return words
}

func goLocalPointerWordIndexes(function *ir.Func, allocations map[*ir.Instr]int, spillBase int) []int {
	seen := make(map[int]bool)
	for _, frameOffset := range goPointerFrameOffsets(function, allocations, spillBase) {
		if frameOffset >= 16 && frameOffset%8 == 0 {
			seen[(frameOffset-16)/8] = true
		}
	}
	words := make([]int, 0, len(seen))
	for word := range seen {
		words = append(words, word)
	}
	sort.Ints(words)
	return words
}

func goPointerFrameOffsets(function *ir.Func, allocations map[*ir.Instr]int, spillBase int) []int {
	seen := make(map[int]bool)
	for instruction, allocationOffset := range allocations {
		for wordOffset := range function.StackPointerWords[instruction.To.ID] {
			frameOffset := allocationOffset + wordOffset
			if frameOffset >= 16 && frameOffset%8 == 0 {
				seen[frameOffset] = true
			}
		}
	}
	if function.UsesManagedFrame() {
		for _, temporary := range function.Temps {
			pointer := temporary.GCRef || temporary.Cls == ir.ClsP
			if pointer && temporary.Reg == ir.NoReg && temporary.Slot >= 0 {
				seen[spillBase+temporary.Slot] = true
			}
		}
	}
	offsets := make([]int, 0, len(seen))
	for offset := range seen {
		offsets = append(offsets, offset)
	}
	sort.Ints(offsets)
	return offsets
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
	case size == 1:
		m.emit(a64.LdrbImm(r, base, uint32(off)))
	case size == 2:
		m.emit(a64.LdrhImm(r, base, uint32(off)))
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
	case size == 1:
		m.emit(a64.StrbImm(r, base, uint32(off)))
	case size == 2:
		m.emit(a64.StrhImm(r, base, uint32(off)))
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

// rematOf returns the rematerialisation rule for temp id, if it has one.
func (m *mc) rematOf(id int) (rematRule, bool) {
	if m.alloc.remat == nil {
		return rematRule{}, false
	}
	r, ok := m.alloc.remat[id]
	return r, ok
}

// rematerialize recomputes a rematerialisable value into register r: a stack-slot
// address as x29+offset, a constant via movz/movk, a symbol via adrp+add.
func (m *mc) rematerialize(r a64.Reg, rule rematRule, size int) {
	switch rule.kind {
	case rematAlloca:
		m.frameAddr(r, m.allocOff[rule.in])
	case rematConst:
		m.movImm(r, rule.c.Int, size == 8)
	case rematSym:
		m.materializeSym(r, rule.c)
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
	if ref.Kind == ir.RefAggregate {
		m.fail("arm64: function %s has aggregate operand %v where a scalar is required", m.f.Name, ref)
		return mcIntScratch[slot]
	}
	if ref.Kind == ir.RefTemp {
		if offset, ok := m.allocTmp[ref.ID]; ok {
			scratch := mcIntScratch[slot]
			m.frameAddr(scratch, offset)
			return scratch
		}
	}
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
		if rule, ok := m.rematOf(int(ref.ID)); ok {
			m.rematerialize(scr, rule, size)
			return scr
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

// emitPacia emits PACIA1716: sign the pointer in Args[0] with the modifier in
// Args[1], leaving the result in To. PACIA1716 is defined on x17 (the data) and
// x16 (the modifier), which are cg12's scratch registers and thus outside the
// allocatable pool -- so the operands cannot be delivered through a register-
// pinned asm operand. They are instead loaded directly into x17/x16 here. An
// integer spill load reuses its own target register for the address, so loading
// x17 then x16 never has one clobber the other, and the result is read from x17
// before x16 is reused for the store-back.
func (m *mc) emitPacia(in *ir.Instr) {
	const x16, x17 = a64.Reg(16), a64.Reg(17)
	m.loadReg(x17, in.Args[0]) // data
	m.loadReg(x16, in.Args[1]) // modifier
	m.emit(a64.Hint(8))        // PACIA1716
	d, done := m.dst(in.To, 8)
	m.emitReg(in.Cls, d, x17)
	done()
}

// loadReg materializes ref's value directly into the physical register reg,
// never borrowing a scratch register. It exists for emitPacia, whose target
// registers are themselves the scratch registers m.src would use.
func (m *mc) loadReg(reg a64.Reg, ref ir.Ref) {
	switch ref.Kind {
	case ir.RefTemp:
		t := m.f.Temps[ref.ID]
		if t.Reg != ir.NoReg {
			m.emitReg(ir.ClsL, reg, mreg(Reg(t.Reg)))
			return
		}
		if rule, ok := m.rematOf(int(ref.ID)); ok {
			m.rematerialize(reg, rule, 8)
			return
		}
		m.spillLoad(reg, false, m.spillBase+t.Slot, 8)
		return
	case ir.RefConst:
		switch c := m.f.Consts[ref.ID]; c.Kind {
		case ir.ConstInt:
			m.movImm(reg, c.Int, true)
			return
		case ir.ConstSym:
			m.materializeSym(reg, c)
			return
		}
	}
	m.fail("arm64: cannot materialize pacia operand %v", ref)
}

// --- parallel moves --------------------------------------------------------

func (m *mc) parallelMove(pairs []movePairLoc) {
	(m.newSel()).parallelMove(pairs)
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
	m.currentBlock = b.Name
	m.currentOp = 0
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
	fuseCmp := m.fusableCmp(b)
	for i < len(b.Instrs) {
		in := &b.Instrs[i]
		m.instrPC[in] = uint64(m.prog.Len() * 4)
		m.recordLoc(in.Pos)
		m.recordInline(in.Inl)
		m.currentOp = in.Op
		if in == fuseCmp {
			switch {
			case fuseCmp.Op == ir.OAnd:
				// `if (x & (1<<k))` needs no flags when it becomes a tbnz: the terminator
				// tests the bit on the register directly, so emit nothing here. Any other
				// `if (x & mask)` is a tst (flags-only) the terminator branches on.
				if m.tbzFires(b, fuseCmp) {
					break
				}
				m.newSel().tstFlags(fuseCmp)
			case fuseCmp.Op == ir.OCCmp:
				// A conjoined `cmp1 && cmp2` / `cmp1 || cmp2` is cmp; ccmp (flags
				// only); the terminator branches on the second predicate.
				m.newSel().ccmpFlags(fuseCmp)
			case m.isZeroCmp(fuseCmp):
				// `== 0` / `!= 0` needs no flags: the terminator emits cbz/cbnz on the
				// operand directly, so emit nothing here.
			case !m.reusesPredFlags(b, fuseCmp):
				// Any other comparison, emitted flags-only (no cset), reusing a
				// predecessor's identical comparison when it already set the flags.
				m.newSel().cmpFlags(fuseCmp)
			}
			i++
			continue
		}
		if in.Op == ir.OArg {
			i = m.callSequence(b, i)
			continue
		}
		if n := m.tryPairSpill(b, i); n > 0 {
			i += n
			continue
		}
		if n := m.tryWriteback(b, i); n > 0 {
			i += n
			continue
		}
		m.instr(in)
		i++
	}
	if m.gotTermPC != nil {
		m.gotTermPC[b] = m.prog.Len() * 4
	}
	if fuseCmp != nil {
		m.emitFusedBranch(b, fuseCmp)
		return
	}
	if !m.blockDone {
		m.currentOp = 0
		m.term(b)
	}
}

// tryPairSpill fuses two consecutive OSpill (or two OReload) instructions that
// target adjacent 8-byte integer frame slots into one stp (or ldp), as gcc pairs
// caller-saved spills around a call. callersave emits each run in slot order, so
// two landing in adjacent slots sit next to each other here. It returns the
// number of instructions consumed (2) or 0.
func (m *mc) tryPairSpill(b *ir.Block, i int) int {
	if i+1 >= len(b.Instrs) {
		return 0
	}
	a, c := &b.Instrs[i], &b.Instrs[i+1]
	if a.Op != c.Op || (a.Op != ir.OSpill && a.Op != ir.OReload) {
		return 0
	}
	var t1, t2 *ir.Temp
	if a.Op == ir.OSpill {
		t1, t2 = m.f.Temps[a.Args[0].ID], m.f.Temps[c.Args[0].ID]
	} else {
		t1, t2 = m.f.Temps[a.To.ID], m.f.Temps[c.To.ID]
	}
	if t1.Cls.IsFloat() || t2.Cls.IsFloat() || t1.Cls.Size() != 8 || t2.Cls.Size() != 8 {
		return 0
	}
	off := m.spillBase + int(a.Aux)
	if m.spillBase+int(c.Aux) != off+8 || off%8 != 0 || off < -512 || off > 504 {
		return 0
	}
	r1, r2 := mreg(Reg(t1.Reg)), mreg(Reg(t2.Reg))
	if a.Op == ir.OSpill {
		m.emit(a64.Stp(true, r1, r2, mcX29, off, a64.SignedOffset))
	} else {
		m.emit(a64.Ldp(true, r1, r2, mcX29, off, a64.SignedOffset))
	}
	return 2
}

// fusableCmp reports the block's trailing comparison when it feeds the block's
// own jnz terminator and nothing else -- the shape `t = cmp a, b; jnz t` -- so the
// comparison and branch can be emitted as `cmp; b.cond` instead of materializing a
// boolean with cset and testing it with cbnz. It requires the comparison to be the
// final instruction (so its flags reach the branch unclobbered) and single-use (so
// the boolean is genuinely dead). Returns nil when no fusion applies.
func (m *mc) fusableCmp(b *ir.Block) *ir.Instr {
	if b.Jmp.Kind != ir.JmpJnz || b.Jmp.Arg.Kind != ir.RefTemp {
		return nil
	}
	if len(b.Instrs) == 0 {
		return nil
	}
	if m.useCount[b.Jmp.Arg.ID] != 1 { // the sole use must be the branch
		return nil
	}
	// Find the comparison that defines the branch condition. It need not be the
	// last instruction: SSA destruction can append phi-resolution copies after it
	// (e.g. in a loop header). Those, and anything else between the comparison and
	// the terminator, must preserve the flags -- only a copy/move does, so require
	// exactly that. The branch then reads the comparison's flags across them.
	idx := -1
	for i := range b.Instrs {
		if in := &b.Instrs[i]; in.To.Kind == ir.RefTemp && in.To.ID == b.Jmp.Arg.ID {
			if in.Op != ir.OCmp && in.Op != ir.OCCmp && !m.tstableAnd(in) {
				return nil
			}
			idx = i
		}
	}
	if idx < 0 {
		return nil
	}
	for i := idx + 1; i < len(b.Instrs); i++ {
		if b.Instrs[i].Op != ir.OCopy {
			return nil
		}
	}
	return &b.Instrs[idx]
}

// emitFusedBranch emits a trailing comparison as flags-only and branches on them
// directly: to To when the predicate holds (the jnz's non-zero arm), else to To2.
// When the block's sole predecessor already set the flags with the identical
// comparison (a three-way switch node's ordering test following its equality
// test), the comparison is omitted and only the branch is emitted -- the AArch64
// cmp; b.eq; b.lt idiom, one cmp feeding two conditional branches.
// emitFusedBranch emits the block's terminator as a branch on the flags the
// fused comparison (already emitted flags-only in the block body) set: to To when
// the predicate holds (the jnz's non-zero arm), else to To2.
// cmpZeroReg reports an integer comparison that is an equality test against the
// constant 0 (`x == 0` or `x != 0`), returning x and whether the test is `== 0`.
// Fused into a branch, such a test becomes a single cbz/cbnz on x, rather than
// `cmp x,#0; b.cond`.
func (m *mc) cmpZeroReg(cmp *ir.Instr) (ir.Ref, bool, bool) {
	if cmp.Op != ir.OCmp || (cmp.Cmp != ir.CmpEq && cmp.Cmp != ir.CmpNe) {
		return ir.R, false, false
	}
	if m.f.ClassOf(cmp.Args[0]).IsFloat() {
		return ir.R, false, false // fcmp sets flags; there is no fp cbz
	}
	a, bb := cmp.Args[0], cmp.Args[1]
	if v, ok := intConst(m.f, bb); ok && v == 0 {
		return a, cmp.Cmp == ir.CmpEq, true
	}
	if v, ok := intConst(m.f, a); ok && v == 0 {
		return bb, cmp.Cmp == ir.CmpEq, true
	}
	return ir.R, false, false
}

// tbzInRange reports whether a single-bit test in block b, branching to target,
// can fuse into a tbz/tbnz: its target must be within tbz's +/-32 KiB reach. The
// distance is the pass-1 (tbz-disabled) offset between b's terminator and target's
// start; that pass is a strict upper bound on the pass-2 distance (enabling a tbz
// only removes words between them), so an in-range pass-1 branch is in range now.
// Always false in pass 1 (refTermPC nil), so pass 1 emits no tbz.
func (m *mc) tbzInRange(b, target *ir.Block) bool {
	if m.refTermPC == nil {
		return false
	}
	tp, ok := m.refTermPC[b]
	if !ok {
		return false
	}
	bp, ok := m.refBlockPC[target]
	if !ok {
		return false
	}
	d := tp - bp
	if d < 0 {
		d = -d
	}
	return d <= tbzRangeLimit
}

// tbzRangeLimit is the byte reach used to gate tbz/tbnz fusion. The hardware reach
// is +/-32768; the margin below it absorbs any slack between the pass-1 measurement
// and the shorter pass-2 layout.
const tbzRangeLimit = 30000

// isTbzRangeErr reports whether err is the assembler's out-of-range diagnostic for
// a tbz/tbnz -- the signal to fall back to the tbz-disabled layout.
func isTbzRangeErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "out of range") && (strings.Contains(s, "tbz") || strings.Contains(s, "tbnz"))
}

// tbzBranch reports whether the AND condition `x & (1<<k)` can drive a single tbz/
// tbnz: one operand is a register and the other a constant that is exactly one bit.
// It returns the register operand and the bit index.
func (m *mc) tbzBranch(and *ir.Instr) (ir.Ref, uint32, bool) {
	if and.Op != ir.OAnd || and.Cls.IsFloat() {
		return ir.R, 0, false
	}
	for i := 0; i < 2; i++ {
		if v, ok := intConst(m.f, and.Args[i]); ok {
			u := uint64(v)
			if and.Cls.Size() == 4 {
				u &= 0xffffffff
			}
			if u != 0 && u&(u-1) == 0 { // a single set bit
				bit := 0
				for u > 1 {
					u >>= 1
					bit++
				}
				return and.Args[1-i], uint32(bit), true
			}
			return ir.R, 0, false // a multi-bit mask is a tst, not a tbz
		}
	}
	return ir.R, 0, false
}

// tstableAnd reports whether in is an integer AND that can be emitted as a
// flags-only tst feeding a branch. A constant mask must be a valid logical
// immediate; a register mask always works.
func (m *mc) tstableAnd(in *ir.Instr) bool {
	return tstableAnd(m.f, in)
}

// tstableAnd reports whether in is an integer AND emittable as a flags-only tst.
// A constant mask must be a valid logical immediate; a register mask always
// works. Shared by the compare-branch fusion (mc) and the compare-select fusion
// (lower).
func tstableAnd(f *ir.Func, in *ir.Instr) bool {
	if in.Op != ir.OAnd || in.Cls.IsFloat() {
		return false
	}
	for _, a := range in.Args {
		if v, ok := intConst(f, a); ok {
			sz := int(in.Cls.Size())
			mask := uint64(v)
			if sz == 4 {
				mask &= 0xffffffff
			}
			_, _, _, ok := a64.EncodeBitmask(mask, sz)
			return ok
		}
	}
	return true
}

// isZeroCmp reports whether cmp is an equality test against 0 (see cmpZeroReg).
func (m *mc) isZeroCmp(cmp *ir.Instr) bool {
	_, _, ok := m.cmpZeroReg(cmp)
	return ok
}

// branchArm chooses which arm of b's two-way terminator is branched to and which
// is the fall-through. Normally the branch goes to To (the non-zero arm) and To2
// falls through; but when To is the block laid out next, the branch instead goes to
// To2 (with the condition inverted) so To falls through -- avoiding a branch to the
// block already sitting next. It returns the branch target and whether the condition
// is inverted.
func (m *mc) branchArm(b *ir.Block) (target *ir.Block, invert bool) {
	if b.Jmp.To == m.nextBlock && b.Jmp.To2 != m.nextBlock {
		return b.Jmp.To2, true
	}
	return b.Jmp.To, false
}

// tbzFires reports whether b's fused single-bit test emits as a tbz/tbnz -- so the
// block body needs no flags-setting tst. It must agree with emitFusedBranch's arm
// and range choice, or the body would omit the tst while the terminator falls back
// to a flags branch.
func (m *mc) tbzFires(b *ir.Block, cmp *ir.Instr) bool {
	if _, _, ok := m.tbzBranch(cmp); !ok {
		return false
	}
	target, _ := m.branchArm(b)
	return m.tbzInRange(b, target)
}

func (m *mc) emitFusedBranch(b *ir.Block, cmp *ir.Instr) {
	s := m.newSel()
	To2 := b.Jmp.To2
	target, invert := m.branchArm(b)
	tail := func() {
		if !invert && To2 != m.nextBlock {
			s.b.branch(To2)
		}
	}

	// `if (x & mask)`: a single-bit test is a tbnz/tbz on x directly (in tbz range),
	// else a tst (flags-only, already in the body) branched on being non-zero.
	if cmp.Op == ir.OAnd {
		if reg, bit, ok := m.tbzBranch(cmp); ok && m.tbzInRange(b, target) {
			r := s.src(reg, 0, m.f.ClassOf(reg).Size())
			if invert {
				s.b.tbz(bit, r, target) // fall into the bit-set arm (To)
			} else {
				s.b.tbnz(bit, r, target) // branch to the bit-set arm
			}
			tail()
			return
		}
		cond, _ := condOf(ir.CmpNe, false)
		if invert {
			cond = cond.Invert()
		}
		s.b.bcondTo(cond, target.Name)
		tail()
		return
	}
	// `x == 0` / `x != 0` is a cbz/cbnz on x. To (the non-zero-result arm) is taken
	// on cbnz for `!= 0` and cbz for `== 0`; inverting swaps which of the two we emit.
	if reg, isEq, ok := m.cmpZeroReg(cmp); ok {
		sz := m.f.ClassOf(reg).Size()
		r := s.src(reg, 0, sz)
		if isEq != invert { // == 0 (and not inverted), or != 0 inverted
			s.b.cbzTo(sz == 8, r, target.Name)
		} else {
			s.b.cbnzTo(sz == 8, r, target.Name)
		}
		tail()
		return
	}
	float := m.f.ClassOf(cmp.Args[0]).IsFloat()
	cond, ok := condOf(cmp.Cmp, float)
	if !ok {
		m.fail("arm64: cannot fuse comparison %v into a branch", cmp.Cmp)
		return
	}
	if invert {
		cond = cond.Invert()
	}
	s.b.bcondTo(cond, target.Name)
	tail()
}

// reusesPredFlags reports whether b's fused comparison can read the flags its
// predecessor already set, instead of recomputing them. It holds when b has a
// single predecessor that ends in a fused compare-branch over the identical
// operands, and b contains nothing that disturbs the flags before its own
// comparison -- so on the one path into b (from that predecessor) the
// predecessor's comparison flags still hold. Stream layout is irrelevant: b is
// reached only from that predecessor at run time, whatever blocks sit between them
// in the emission order.
func (m *mc) reusesPredFlags(b *ir.Block, cmp *ir.Instr) bool {
	if len(cmp.Args) != 2 {
		return false
	}
	for i := range b.Instrs {
		if in := &b.Instrs[i]; in != cmp && in.Op != ir.OCopy {
			return false // a flag-clobbering instruction reaches b's branch first
		}
	}
	preds := m.preds[b]
	if len(preds) != 1 {
		return false
	}
	pc := m.fusableCmp(preds[0])
	// Only a real comparison leaves reusable subtraction flags. An AND fuses to
	// tst (different flag meaning) and an `== 0`/`!= 0` fuses to cbz/cbnz (no flags
	// at all), so neither predecessor can lend its flags to b's comparison.
	if pc == nil || pc.Op != ir.OCmp || m.isZeroCmp(pc) || len(pc.Args) != 2 {
		return false
	}
	// Same operands in the same order produce the same cmp, so its flags answer
	// b's comparison too (a different predicate reads different flag bits).
	return pc.Args[0] == cmp.Args[0] && pc.Args[1] == cmp.Args[1] &&
		m.f.ClassOf(pc.Args[0]) == m.f.ClassOf(cmp.Args[0])
}

// blockPreds returns each block's predecessors, following every terminator's
// explicit successors (jumps, the two arms of a conditional, switch/table arms).
func blockPreds(f *ir.Func) map[*ir.Block][]*ir.Block {
	preds := map[*ir.Block][]*ir.Block{}
	for _, b := range f.Blocks {
		for _, s := range b.Succs() {
			preds[s] = append(preds[s], b)
		}
	}
	return preds
}

// condOf maps a comparison predicate to its AArch64 branch condition, choosing the
// integer or floating-point mapping.
func condOf(c ir.Cmp, float bool) (a64.Cond, bool) {
	if float {
		return fpCondOf(c)
	}
	return intCondOf(c)
}

// countTempUses counts, per temp, how many times it appears as an operand (in any
// instruction's Args or a block terminator's arguments). Definitions do not count.
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

// tryWriteback folds a pointer-advancing load or store together with the add/sub
// that advances the pointer, emitting a single pre/post-indexed access:
//
//	ldrb w, [pc] ; add pc, pc, #1   ->   ldrb w, [pc], #1     (post-index)
//	sub sp, sp, #8 ; ldr x, [sp]     ->   ldr x, [sp, #-8]!    (pre-index)
//
// It returns the number of instructions consumed (2) or 0. The safety proof is the
// register equality: the allocator placed the pointer update in the base's own
// register, so the base's pre-update value is dead past it, and b is reached only
// along this straight run so nothing else observes the intermediate pointer.
func (m *mc) tryWriteback(b *ir.Block, i int) int {
	if i+1 >= len(b.Instrs) {
		return 0
	}
	x, y := &b.Instrs[i], &b.Instrs[i+1]

	// Post-index: access [base]; then base += delta.
	if base, dv, ok := wbAccess(x); ok {
		if br, ok := m.physReg(base); ok && m.regDiffers(dv, br) {
			if reg, delta, ok := m.selfIncrement(y); ok && reg == br {
				if m.emitWriteback(x, base, dv, delta, a64.Post) {
					return 2
				}
			}
		}
	}
	// Pre-index: base += delta; then access [base].
	if reg, delta, ok := m.selfIncrement(x); ok {
		if base, dv, ok := wbAccess(y); ok {
			if br, ok := m.physReg(base); ok && br == reg && m.regDiffers(dv, br) {
				if m.emitWriteback(y, base, dv, delta, a64.Pre) {
					return 2
				}
			}
		}
	}
	return 0
}

// wbAccess reports a simple base-only load or store (offset zero, no index) as a
// write-back candidate: base is the pointer, dv the value loaded or stored (which
// must not share the base register).
func wbAccess(in *ir.Instr) (base, dv ir.Ref, ok bool) {
	if in.Aux != 0 {
		return ir.Ref{}, ir.Ref{}, false
	}
	switch {
	case in.Op.IsLoad() && len(in.Args) == 1:
		return in.Args[0], in.To, true
	case in.Op.IsStore() && len(in.Args) == 2:
		return in.Args[1], in.Args[0], true
	}
	return ir.Ref{}, ir.Ref{}, false
}

// selfIncrement reports an add/sub of a register by a small constant back into the
// same register (base += delta), the shape a write-back folds in. delta is a
// signed byte offset in [-256, 255].
func (m *mc) selfIncrement(in *ir.Instr) (Reg, int32, bool) {
	if (in.Op != ir.OAdd && in.Op != ir.OSub) || len(in.Args) != 2 {
		return 0, 0, false
	}
	to, ok := m.physReg(in.To)
	if !ok {
		return 0, 0, false
	}
	if a0, ok := m.physReg(in.Args[0]); !ok || a0 != to {
		return 0, 0, false
	}
	c, ok := intConst(m.f, in.Args[1])
	if !ok {
		return 0, 0, false
	}
	if in.Op == ir.OSub {
		c = -c
	}
	if c < -256 || c > 255 {
		return 0, 0, false
	}
	return to, int32(c), true
}

// emitWriteback emits access as a pre/post-indexed load or store; it declines
// (returns false) for FP or other forms the write-back encoders do not cover.
func (m *mc) emitWriteback(access *ir.Instr, base, dv ir.Ref, delta int32, wb a64.WriteBack) bool {
	s := m.newSel()
	br, _ := m.physReg(base)
	vr, _ := m.physReg(dv)
	if access.Op.IsLoad() {
		return s.b.loadWB(access.Op, access.Cls, vr, br, delta, wb)
	}
	return s.b.storeWB(access.Op, vr, br, delta, wb)
}

// physReg is the physical register a temp is bound to, or ok=false when it is a
// non-temp, spilled, or rematerialised (no register to write back through).
func (m *mc) physReg(r ir.Ref) (Reg, bool) {
	if r.Kind != ir.RefTemp {
		return 0, false
	}
	t := m.f.Temps[r.ID]
	if t == nil || t.Reg == ir.NoReg {
		return 0, false
	}
	return Reg(t.Reg), true
}

// regDiffers reports that r is in a register other than avoid -- a write-back must
// not name the base as its data register (constrained unpredictable on AArch64).
func (m *mc) regDiffers(r ir.Ref, avoid Reg) bool {
	rr, ok := m.physReg(r)
	return ok && rr != avoid
}

func (m *mc) stackParam(in *ir.Instr) {
	off := m.frameTop() + int(in.Aux) + stackLinkBytesFor(m.f.UsesGoInternalCallConvention())
	t := m.f.Temps[in.To.ID]
	if t.Agg != nil {
		if in.RetAgg != nil {
			if t.Reg != ir.NoReg {
				m.frameAddr(mreg(Reg(t.Reg)), off)
				return
			}
			m.frameAddr(mcGP0, off)
			m.spillStore(mcGP0, false, m.spillBase+t.Slot, 8)
			return
		}
		if t.Reg != ir.NoReg {
			m.spillFromFrame(mreg(Reg(t.Reg)), false, off, 8)
			return
		}
		m.spillFromFrame(mcGP0, false, off, 8)
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
	var stackArgs, symArgs, frameArgs []*ir.Instr
	for i < len(b.Instrs) && b.Instrs[i].Op == ir.OArg {
		a := &b.Instrs[i]
		switch {
		case a.To.IsNone():
			stackArgs = append(stackArgs, a)
		case isConstSym(m.f, a.Args[0]):
			symArgs = append(symArgs, a)
		case a.Args[0].Kind == ir.RefTemp && m.hasFrameAllocation(a.Args[0]):
			frameArgs = append(frameArgs, a)
		default:
			regPairs = append(regPairs, movePairLoc{dst: m.locOf(a.To), src: m.locOf(a.Args[0])})
		}
		i++
	}
	call := &b.Instrs[i]
	goInternal := m.conventions.goInternalCall(m.f, call)
	m.instrPC[call] = uint64(m.prog.Len() * 4)
	m.recordLoc(call.Pos) // the OArg run carries no position; the call does
	m.recordInline(call.Inl)

	if call.Tail {
		for _, a := range stackArgs {
			r := m.src(a.Args[0], 0, a.Cls.Size())
			offset := m.frameTop() + int(a.Aux) + stackLinkBytesFor(goInternal)
			m.emit(a64.StrImm(a.Cls.Size() == 8, r, mcX29, uint32(offset)))
		}
		m.parallelMove(regPairs)
		for _, a := range symArgs {
			m.materializeSym(mreg(Reg(m.f.Temps[a.To.ID].Reg)), m.f.Consts[a.Args[0].ID])
		}
		for _, a := range frameArgs {
			m.materializeFrameAllocation(a)
		}
		m.tailBranch(call)
		m.blockDone = true
		return i + 1
	}

	stackBytes := int(call.Aux)
	dynamicAAPCSFrame := stackBytes > 0 && !goInternal && !m.f.UsesManagedFrame()
	if dynamicAAPCSFrame {
		m.adjustSP(true, stackBytes)
	}
	for _, a := range stackArgs {
		if a.RetAgg != nil {
			m.emitStackAggregateArgument(a, goInternal)
			continue
		}
		sz := a.Cls.Size()
		r := m.src(a.Args[0], 0, sz)
		offset := int(a.Aux) + stackLinkBytesFor(goInternal)
		if a.Cls.IsFloat() {
			m.emit(a64.StrFP(sz == 8, r, mcSP, uint32(offset)))
		} else {
			m.emit(a64.StrImm(sz == 8, r, mcSP, uint32(offset)))
		}
	}
	m.parallelMove(regPairs)
	for _, a := range symArgs {
		m.materializeSym(mreg(Reg(m.f.Temps[a.To.ID].Reg)), m.f.Consts[a.Args[0].ID])
	}
	for _, a := range frameArgs {
		m.materializeFrameAllocation(a)
	}
	m.emitCall(call)
	if call.RetAgg != nil && !call.StackResult.IsNone() {
		m.emitStackAggregateResult(call)
	}
	if dynamicAAPCSFrame {
		m.adjustSP(false, stackBytes)
	}
	return i + 1
}

func (m *mc) hasFrameAllocation(reference ir.Ref) bool {
	_, ok := m.allocTmp[reference.ID]
	return ok
}

func (m *mc) materializeFrameAllocation(argument *ir.Instr) {
	offset := m.allocTmp[argument.Args[0].ID]
	destination := m.f.Temp(argument.To)
	if destination.Reg == ir.NoReg {
		m.fail("arm64: stack-allocation argument has no ABI register")
		return
	}
	m.frameAddr(mreg(Reg(destination.Reg)), offset)
}

func (m *mc) emitStackAggregateArgument(argument *ir.Instr, goInternal bool) {
	source := m.src(argument.Args[0], 0, 8)
	if source != mcGP2 {
		m.emit(a64.AddImm(true, mcGP2, source, 0))
	}
	size, _ := argument.RetAgg.Layout()
	destinationOffset := int(argument.Aux) + stackLinkBytesFor(goInternal)
	m.emitStackAggregateCopy(mcSP, destinationOffset, mcGP2, 0, size)
}

func (m *mc) emitStackAggregateResult(call *ir.Instr) {
	destination := m.src(call.StackResult, 0, 8)
	if destination != mcGP2 {
		m.emit(a64.AddImm(true, mcGP2, destination, 0))
	}
	size, _ := call.RetAgg.Layout()
	m.emitStackAggregateCopy(mcGP2, 0, mcSP, goStackLinkSize+int(call.StackResultOffset), size)
}

func (m *mc) emitStackAggregateCopy(destination a64.Reg, destinationOffset int, source a64.Reg, sourceOffset int, size int) {
	for offset := 0; offset < size; {
		remaining := size - offset
		sourceAddress := sourceOffset + offset
		destinationAddress := destinationOffset + offset
		width := 1
		for _, candidate := range []int{8, 4, 2} {
			if remaining >= candidate && sourceAddress%candidate == 0 && destinationAddress%candidate == 0 {
				width = candidate
				break
			}
		}
		switch width {
		case 8:
			m.emit(a64.LdrImm(true, mcGP0, source, uint32(sourceAddress)))
			m.emit(a64.StrImm(true, mcGP0, destination, uint32(destinationAddress)))
		case 4:
			m.emit(a64.LdrImm(false, mcGP0, source, uint32(sourceAddress)))
			m.emit(a64.StrImm(false, mcGP0, destination, uint32(destinationAddress)))
		case 2:
			m.emit(a64.LdrhImm(mcGP0, source, uint32(sourceAddress)))
			m.emit(a64.StrhImm(mcGP0, destination, uint32(destinationAddress)))
		case 1:
			m.emit(a64.LdrbImm(mcGP0, source, uint32(sourceAddress)))
			m.emit(a64.StrbImm(mcGP0, destination, uint32(destinationAddress)))
		}
		offset += width
	}
}

func (m *mc) emitCall(in *ir.Instr) {
	(m.newSel()).call(in)
	m.recordSafepoint(in) // the branch just emitted; the return address is next
}

// recordSafepoint records the GC roots live at the safepoint in, at the current
// PC, reading each root's location from its register/spill-slot binding. For a
// call this is invoked just after the branch (so the PC is the return address);
// for an explicit OSafepoint it is invoked at the marker (which emits no code).
func (m *mc) recordSafepoint(in *ir.Instr) {
	roots := m.alloc.safeRoots[in]
	undefined := m.alloc.undefinedAllocs[in]
	locs := make([]rootLoc, 0, len(roots))
	for _, id := range roots {
		t := m.f.Temps[id]
		// An allocation the program has not written since it was entered holds the
		// previous incarnation's words; describing them here hands the collector a
		// pointer the program abandoned. The allocation's own address is still
		// reported below, so stack copying keeps relocating it.
		if allocationOffset, ok := m.stackAllocTmp[uint32(id)]; ok && !undefined[id] {
			for wordOffset := range m.f.StackPointerWords[uint32(id)] {
				locs = append(locs, rootLoc{
					kind: rootFrame,
					val:  int32(allocationOffset + wordOffset),
				})
			}
		}
		if t.Reg != ir.NoReg {
			locs = append(locs, rootLoc{kind: rootReg, val: int32(mreg(Reg(t.Reg))), typ: t.GCType})
		} else {
			locs = append(locs, rootLoc{kind: rootFrame, val: int32(m.spillBase + t.Slot), typ: t.GCType})
		}
	}
	startPC, found := m.instrPC[in]
	if !found {
		startPC = uint64(m.prog.Len() * 4)
	}
	m.safepoints = append(m.safepoints, safepoint{
		pc:      uint64(m.prog.Len() * 4),
		startPC: startPC,
		roots:   locs,
	})
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

// newSel builds a selection driver over the machine-code encoder, carrying the
// rematerialisation table so spilled-but-recomputable operands are rebuilt in place.
func (m *mc) newSel() *sel {
	s := &sel{
		f:         m.f,
		b:         &mcAsm{prog: m.prog, m: m},
		spillBase: m.spillBase,
		next:      m.nextBlock,
		allocTmp:  m.allocTmp,
	}
	if m.alloc != nil {
		s.remat = m.alloc.remat
	}
	return s
}

func (m *mc) instr(in *ir.Instr) {
	// A rematerialisable result (a spilled alloca address or constant with no slot)
	// emits nothing at its definition -- every use recomputes it.
	if in.To.Kind == ir.RefTemp && m.f.Temps[in.To.ID].Reg == ir.NoReg {
		if _, ok := m.rematOf(int(in.To.ID)); ok {
			return
		}
	}
	// Integer data-processing instructions are selected once, through the shared
	// builder (here backed by the machine-code encoder).
	if (m.newSel()).selectData(in) {
		return
	}
	switch in.Op {
	case ir.ONop:
		return
	case ir.OSpill:
		// Save a caller-saved value to its slot before a call it is live across.
		t := m.f.Temps[in.Args[0].ID]
		m.spillStore(mreg(Reg(t.Reg)), t.Cls.IsFloat(), m.spillBase+int(in.Aux), t.Cls.Size())
	case ir.OReload:
		// Restore it into the same register after the call.
		t := m.f.Temps[in.To.ID]
		m.spillLoad(mreg(Reg(t.Reg)), t.Cls.IsFloat(), m.spillBase+int(in.Aux), t.Cls.Size())
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
	case ir.OPacia:
		m.emitPacia(in)
	case ir.OSafepoint:
		// Let the GC strategy emit code here (e.g. a poll); the stack map is then
		// recorded at the resulting PC — the point a collection would resume at.
		if m.gc != nil {
			m.gc.EmitSafepoint(&GCContext{mc: m, roots: m.gcRoots(in)})
		}
		m.recordSafepoint(in)
	case ir.OLifeStart, ir.OLifeEnd:
		// Alloca lifetime markers carry no code; they only bound live ranges and
		// stack-slot lifetimes for earlier passes.
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
	if len(in.Args) != 1 {
		m.fail("arm64: %s to %v has %d operands, want 1", in.Op, in.To, len(in.Args))
		return
	}
	sz := in.Cls.Size()
	// An integer-constant source materializes straight into the destination register.
	// The general src path would load it into a scratch and then move it (movImm into
	// x16, then `mov dst, x16`); a copy of a constant is the common phi-destruction
	// case, so the extra move showed up on nearly a thousand sites.
	if c, ok := intConst(m.f, in.Args[0]); ok {
		d, done := m.dst(in.To, sz)
		m.movImm(d, c, sz == 8)
		done()
		return
	}
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
		m.fail("arm64: unsupported comparison predicate %v for %s operands", in.Cmp, argCls)
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
	if len(in.Args) == 2 { // indexed: [base, index] with extend/scale in Amode
		base := m.src(in.Args[0], 1, 8)
		option, s := decodeAmode(in.Amode)
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
		option, s := decodeAmode(in.Amode)
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
// in a memory op's Amode.
func decodeAmode(amode int32) (option, s uint32) {
	return uint32(amode>>1) & 7, uint32(amode & 1)
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

// accessWidth is the number of bytes a load or store touches in memory, distinct
// from the register width (a byte load targets a 32-bit W register). It selects
// the scaled-offset relocation when a symbol's low bits fold into the access.
func accessWidth(op ir.Op) int {
	switch op {
	case ir.OLoadub, ir.OLoadsb, ir.OStoreb:
		return 1
	case ir.OLoaduh, ir.OLoadsh, ir.OStoreh:
		return 2
	case ir.OLoaduw, ir.OLoadsw, ir.OStorew, ir.OLoads, ir.OStores:
		return 4
	case ir.OLoadl, ir.OLoadd, ir.OStorel, ir.OStored:
		return 8
	case ir.OLoadq, ir.OStoreq:
		return 16
	}
	return 0
}

// ldstLo12Reloc is the low-12 relocation for a memory access of the given width,
// or 0 for an unsupported one.
func ldstLo12Reloc(bytes int) uint32 {
	switch bytes {
	case 1:
		return obj.R_AARCH64_LDST8_ABS_LO12_NC
	case 2:
		return obj.R_AARCH64_LDST16_ABS_LO12_NC
	case 4:
		return obj.R_AARCH64_LDST32_ABS_LO12_NC
	case 8:
		return obj.R_AARCH64_LDST64_ABS_LO12_NC
	case 16:
		return obj.R_AARCH64_LDST128_ABS_LO12_NC
	}
	return 0
}

func (m *mc) term(b *ir.Block) {
	if (m.newSel()).term(b) {
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
	m.frameAddr(s, m.frameTop()+roundUp(m.namedStack, 8))
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
	clobbered := make(map[Reg]bool)
	for _, register := range asmClobberRegs(in) {
		clobbered[register] = true
	}
	integerScratch := availableAsmScratch(intScratchRegs[:], clobbered)
	floatScratch := availableAsmScratch(floatScratchRegs[:], clobbered)
	integerScratchIndex := 0
	floatScratchIndex := 0
	nextScratch := func(floatingPoint bool) Reg {
		registers := integerScratch
		index := &integerScratchIndex
		kind := "integer"
		fallback := scratch0
		if floatingPoint {
			registers = floatScratch
			index = &floatScratchIndex
			kind = "floating-point"
			fallback = fscratch0
		}
		if *index >= len(registers) {
			m.fail("arm64: inline asm needs more non-clobbered %s scratch registers than are available", kind)
			return fallback
		}
		register := registers[*index]
		(*index)++
		return register
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
		r := nextScratch(m.f.ClassOf(oref).IsFloat())
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
			switch {
			case r.IsFloat() && w == 16:
				m.emit(a64.MovVec16b(mreg(r), mreg(pre)))
			case r.IsFloat():
				m.emit(a64.FmovReg(w == 8, mreg(r), mreg(pre)))
			default:
				m.emit(a64.MovReg(w == 8, mreg(r), mreg(pre)))
			}
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
	prog, err := a64.AssembleProgram(text)
	if err != nil {
		m.fail("arm64: inline assembly: %v", err)
		return
	}
	code, asmRelocs, err := prog.Link()
	if err != nil {
		m.fail("arm64: inline assembly: %v", err)
		return
	}
	// A template that references a symbol (adrp/adr/add :lo12:, or a bl to another
	// function) produces relocations against its own bytes. Re-anchor each to where
	// the template lands in this function's text, so the linker patches the real
	// instruction rather than a zero immediate that dereferences nothing.
	base := m.prog.Len() * 4
	for _, r := range asmRelocs {
		m.relocs = append(m.relocs, obj.Reloc{
			Offset: uint64(base + r.Offset), Sym: sanitize(r.Sym), Type: aarch64RelType(r.Kind),
		})
	}
	for i := 0; i+4 <= len(code); i += 4 {
		m.emit(binary.LittleEndian.Uint32(code[i:]))
	}
	for _, f := range finals {
		f()
	}
}

func availableAsmScratch(candidates []Reg, clobbered map[Reg]bool) []Reg {
	available := make([]Reg, 0, len(candidates))
	for _, register := range candidates {
		if !clobbered[register] {
			available = append(available, register)
		}
	}
	return available
}

// asmInputReg yields a register holding an inline-asm operand's value, loading a
// spilled temporary or materializing a constant into a scratch register first.
func (m *mc) asmInputReg(ref ir.Ref, nextScratch func(bool) Reg) (Reg, int) {
	switch ref.Kind {
	case ir.RefTemp:
		t := m.f.Temps[ref.ID]
		w := m.f.ClassOf(ref).Size()
		if t.Reg != ir.NoReg {
			return Reg(t.Reg), w
		}
		r := nextScratch(m.f.ClassOf(ref).IsFloat())
		m.spillLoad(mreg(r), r.IsFloat(), m.spillBase+t.Slot, w)
		return r, w
	case ir.RefConst:
		c := m.f.Consts[ref.ID]
		r := nextScratch(false)
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
