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
}

// MapReloc records a two-slot ld_imm64 instruction whose immediate must be
// patched with a map's file descriptor at load time.
type MapReloc struct {
	Insn int    // index of the ld_imm64's first slot
	Map  string // referenced map name
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

// Object is a whole compiled module: the maps it needs and the programs that use
// them.
type Object struct {
	Maps     []MapDef
	Programs []*Program
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
	obj := &Object{}
	names := map[string]bool{}
	for _, d := range m.Data {
		if d.Linkage.Section != "maps" {
			continue
		}
		obj.Maps = append(obj.Maps, mapDefFrom(d))
		names[d.Name] = true
	}

	for _, f := range m.Funcs {
		if f.Start == nil {
			continue // a declaration with no body
		}
		c := newComp(f, names)
		if err := c.run(); err != nil {
			return nil, err
		}
		obj.Programs = append(obj.Programs, &Program{
			Name:    f.Name,
			Section: f.Linkage.Section,
			Insns:   c.insns,
			Relocs:  c.relocs,
		})
	}
	return obj, nil
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
