package bpf

import (
	"encoding/binary"

	"github.com/evanphx/cg12/ir"
)

// MapDef describes an eBPF map to create before loading a program. The fields
// mirror the arguments of BPF_MAP_CREATE.
type MapDef struct {
	Name       string
	Type       uint32 // BPF_MAP_TYPE_* (2 = array, 1 = hash, ...)
	KeySize    uint32
	ValueSize  uint32
	MaxEntries uint32
	Flags      uint32

	// Initial, when set, is written into the map's single entry. It backs an
	// internal data section (.rodata or .data), which programs read (and, for
	// .data, write) through map-value addresses.
	Initial []byte
	// Frozen marks the map read-only to programs after Initial is written (used
	// for .rodata, so the verifier treats the data as constant).
	Frozen bool
}

// MapReloc records a two-slot ld_imm64 instruction that references a map or a
// read-only global. The direct loader patches the map's file descriptor into the
// immediate; the ELF emitter turns it into an R_BPF_64_64 relocation on Sym.
type MapReloc struct {
	Insn int    // index of the ld_imm64's first slot
	Map  string // the map to take a descriptor from (".rodata" for global data)
	Sym  string // the referenced symbol (the map, or the specific global for data)
}

// Program is one compiled eBPF program: its bytecode, the ELF-style section that
// names its attach point (e.g. "xdp", "kprobe/sys_execve"), and its map fixups.
type Program struct {
	Name    string
	Section string
	Insns   []Insn
	Relocs  []MapReloc
}

// Bytes returns the program's encoded bytecode.
func (p *Program) Bytes() []byte { return (&Prog{Insns: p.Insns}).Bytes() }

// Asm returns the program's disassembly.
func (p *Program) Asm() string { return (&Prog{Insns: p.Insns}).Asm() }

// DataVar locates one global within a data section (.rodata or .data), for the
// ELF's BTF DATASEC and symbols.
type DataVar struct {
	Name    string
	Section string
	Off     uint32
	Size    uint32
}

// CallReloc records a BPF-to-BPF call to another function by name (for the ELF's
// R_BPF_64_32 relocations).
type CallReloc struct {
	Insn int
	Func string
}

// CompiledFunc is one lowered function kept for the ELF emitter: an entry
// program (Section set) or a subprogram (Section empty), with its map and call
// relocations at per-function instruction positions.
type CompiledFunc struct {
	Name       string
	Section    string
	Insns      []Insn
	MapRelocs  []MapReloc
	CallRelocs []CallReloc
}

// Object is a whole compiled module: the maps it needs and the programs that use
// them.
type Object struct {
	Maps     []MapDef
	Programs []*Program     // entry programs, each linked with its subprograms (for the direct loader)
	Funcs    []CompiledFunc // every function, for the ELF's .text + relocations
	DataVars []DataVar      // layout of the .rodata / .data sections (for the ELF)
	License  string         // from the "license" global, or "GPL"
}

// MapByName returns the map with the given name.
func (o *Object) MapByName(name string) (MapDef, bool) {
	for _, m := range o.Maps {
		if m.Name == name {
			return m, true
		}
	}
	return MapDef{}, false
}

// CompileModule lowers a whole module to eBPF: it reads map definitions from
// globals in the "maps" section, then compiles each function that has a body,
// resolving &map references to relocated map-fd loads.
func CompileModule(m *ir.Module) (*Object, error) {
	obj := &Object{License: "GPL"}
	names := map[string]bool{}
	dataOff := map[string]dataRef{}

	// A global is data (bound for .rodata or .data) unless it is a map or license.
	isData := map[string]bool{}
	for _, d := range m.Data {
		switch d.Linkage.Section {
		case "maps":
			obj.Maps = append(obj.Maps, mapDefFrom(d))
			names[d.Name] = true
		case "license":
			obj.License = trimNUL(string(dataImage(d, 0)))
		default:
			isData[d.Name] = true
		}
	}

	// Only globals a program actually references get storage (this drops unused
	// synthetics like the compiler's __func__ strings).
	used := map[string]bool{}
	for _, f := range m.Funcs {
		if f.Start == nil {
			continue
		}
		for _, c := range f.Consts {
			if c.Kind == ir.ConstSym && isData[c.Sym] {
				used[c.Sym] = true
			}
		}
	}
	blobs := map[string][]byte{}
	for _, d := range m.Data {
		if !used[d.Name] {
			continue
		}
		sec := dataName // mutable by default; const globals and strings are .rodata
		if d.Linkage.Section == rodataName {
			sec = rodataName
		}
		blob := blobs[sec]
		blob = append(blob, make([]byte, align8(len(blob))-len(blob))...)
		off := int32(len(blob))
		img := dataImage(d, int(d.Align))
		blob = append(blob, img...)
		blobs[sec] = blob
		dataOff[d.Name] = dataRef{sec: sec, off: off}
		obj.DataVars = append(obj.DataVars, DataVar{Name: d.Name, Section: sec, Off: uint32(off), Size: uint32(len(img))})
	}
	for _, sec := range []string{rodataName, dataName} {
		if len(blobs[sec]) == 0 {
			continue
		}
		obj.Maps = append(obj.Maps, MapDef{
			Name: sec, Type: 2 /*ARRAY*/, KeySize: 4,
			ValueSize: uint32(len(blobs[sec])), MaxEntries: 1, Initial: blobs[sec], Frozen: sec == rodataName,
		})
	}

	// Compile every function with a body. A call to one of these names is a
	// BPF-to-BPF call rather than a helper.
	funcNames := map[string]bool{}
	for _, f := range m.Funcs {
		if f.Start != nil {
			funcNames[f.Name] = true
		}
	}
	compiled := map[string]*compiledFunc{}
	for _, f := range m.Funcs {
		if f.Start == nil {
			continue
		}
		c := newComp(f, names, dataOff, funcNames)
		if err := c.run(); err != nil {
			return nil, err
		}
		compiled[f.Name] = &compiledFunc{insns: c.insns, relocs: c.relocs, subs: c.subRelocs}
		calls := make([]CallReloc, len(c.subRelocs))
		for i, s := range c.subRelocs {
			calls[i] = CallReloc{Insn: s.Insn, Func: s.Func}
		}
		obj.Funcs = append(obj.Funcs, CompiledFunc{
			Name: f.Name, Section: f.Linkage.Section,
			Insns: c.insns, MapRelocs: c.relocs, CallRelocs: calls,
		})
	}

	// Each function with an attach section is an entry program; link it with the
	// subprograms it (transitively) calls.
	for _, f := range m.Funcs {
		if f.Start == nil || f.Linkage.Section == "" {
			continue
		}
		obj.Programs = append(obj.Programs, linkProgram(f, compiled))
	}
	return obj, nil
}

// compiledFunc is one function's lowered code, before it is linked into a
// program together with its subprograms.
type compiledFunc struct {
	insns  []Insn
	relocs []MapReloc
	subs   []subReloc
}

// linkProgram concatenates an entry function with the subprograms it reaches
// (entry first) and patches every BPF-to-BPF call with its pc-relative offset,
// adjusting map/rodata relocations to their positions in the joined stream.
func linkProgram(entry *ir.Func, compiled map[string]*compiledFunc) *Program {
	order := []string{entry.Name}
	seen := map[string]bool{entry.Name: true}
	for i := 0; i < len(order); i++ {
		cf := compiled[order[i]]
		if cf == nil {
			continue
		}
		for _, s := range cf.subs {
			if !seen[s.Func] {
				seen[s.Func] = true
				order = append(order, s.Func)
			}
		}
	}

	start := map[string]int{}
	var insns []Insn
	var relocs []MapReloc
	for _, name := range order {
		cf := compiled[name]
		start[name] = len(insns)
		for _, r := range cf.relocs {
			r.Insn += start[name]
			relocs = append(relocs, r)
		}
		insns = append(insns, cf.insns...)
	}
	for _, name := range order {
		cf := compiled[name]
		for _, s := range cf.subs {
			at := start[name] + s.Insn
			insns[at].Imm = int32(start[s.Func] - (at + 1))
		}
	}
	return &Program{Name: entry.Name, Section: entry.Linkage.Section, Insns: insns, Relocs: relocs}
}

func align8(n int) int { return (n + 7) &^ 7 }

func trimNUL(s string) string {
	if i := indexByte(s, 0); i >= 0 {
		return s[:i]
	}
	return s
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// mapDefFrom reads a bpf_map_def-shaped global (five u32 fields: type, key_size,
// value_size, max_entries, flags) into a MapDef.
func mapDefFrom(d *ir.Data) MapDef {
	b := dataImage(d, 20)
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }
	return MapDef{
		Name:       d.Name,
		Type:       u32(0),
		KeySize:    u32(4),
		ValueSize:  u32(8),
		MaxEntries: u32(12),
		Flags:      u32(16),
	}
}

// dataImage renders a data definition's items into at least n bytes of raw
// image (little-endian for integer items).
func dataImage(d *ir.Data, n int) []byte {
	var out []byte
	for _, it := range d.Items {
		switch {
		case it.Zero > 0:
			out = append(out, make([]byte, it.Zero)...)
		case it.Str != "":
			out = append(out, it.Str...)
		case len(it.Ints) > 0:
			sz := it.Sub.Size()
			if sz == 0 {
				sz = 8
			}
			for _, v := range it.Ints {
				var buf [8]byte
				binary.LittleEndian.PutUint64(buf[:], uint64(v))
				out = append(out, buf[:sz]...)
			}
		}
	}
	if len(out) < n {
		out = append(out, make([]byte, n-len(out))...)
	}
	return out
}
