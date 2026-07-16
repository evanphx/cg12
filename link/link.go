// Package link combines relocatable objects — compiled from cg12 IR modules or
// read from architecture .o files — into a single object, resolving references
// between them. It performs a partial (incremental) link: the merged sections,
// symbols, and relocations form another relocatable object that a final linker
// can turn into an executable, but cross-object calls that can be resolved now
// (PC-relative branches within the combined .text) are patched and dropped.
package link

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// Backend is the common code-generator output the linker consumes: it compiles
// an IR module to a relocatable object, synthesizes a process-entry stub, and
// names the ELF machine it targets. arm64.Backend and amd64.Backend implement it,
// so the linker handles either architecture through one interface without
// importing its entry points.
type Backend interface {
	Machine() uint16
	CompileModule(m *ir.Module) (*obj.Object, error)
	StartStub(entry string) (*obj.Object, error)
}

// Linker accumulates input objects from either front-end and links them.
type Linker struct {
	be   Backend
	objs []*obj.Object
}

// New returns a linker that compiles IR modules with the arm64 backend.
func New() *Linker { return NewWith(arm64.Backend{}) }

// NewWith returns a linker that compiles IR modules with the given backend, so a
// caller can link amd64 (or any future architecture) output through the same API.
func NewWith(be Backend) *Linker { return &Linker{be: be} }

// AddModule compiles an IR module to a relocatable object and adds it.
func (l *Linker) AddModule(m *ir.Module) error {
	o, err := l.be.CompileModule(m)
	if err != nil {
		return err
	}
	l.objs = append(l.objs, o)
	return nil
}

// AddObjectFile parses an ELF relocatable object (.o) and adds it.
func (l *Linker) AddObjectFile(data []byte) error {
	o, err := obj.ReadELF(data)
	if err != nil {
		return err
	}
	l.objs = append(l.objs, o)
	return nil
}

// AddObject adds an already-built object.
func (l *Linker) AddObject(o *obj.Object) { l.objs = append(l.objs, o) }

// Link merges the inputs into one relocatable object.
func (l *Linker) Link() (*obj.Object, error) { return merge(l.objs) }

// LinkToELF links and serializes the result to a relocatable-object ELF.
func (l *Linker) LinkToELF() ([]byte, error) {
	o, err := l.Link()
	if err != nil {
		return nil, err
	}
	return o.MarshalELF()
}

// LinkExecutable fully links the accumulated objects into a static ELF executable
// that runs directly on Linux with no C runtime or external linker: it synthesizes
// a `_start` stub (via the backend) that calls entry and exits with its return
// value, merges everything, resolves all relocations at their final virtual
// addresses, and writes an ET_EXEC image. entry names the function `_start` calls
// (e.g. "main"), which must be among the linked objects.
func (l *Linker) LinkExecutable(entry string) ([]byte, error) {
	stub, err := l.be.StartStub(entry)
	if err != nil {
		return nil, err
	}
	merged, err := merge(append([]*obj.Object{stub}, l.objs...))
	if err != nil {
		return nil, err
	}
	return merged.WriteExecutable("_start")
}

// merge concatenates the objects' .text/.data, resolves their symbols, re-bases
// their relocations, and applies the intra-.text PC-relative ones.
func merge(objs []*obj.Object) (*obj.Object, error) {
	out := &obj.Object{Machine: obj.EM_AARCH64}
	if len(objs) > 0 {
		out.Machine = objs[0].Machine
	}

	defined := map[string]obj.Sym{} // merged name -> defined symbol
	used := map[string]bool{}
	type pending struct {
		r      obj.Reloc
		toData bool
	}
	var relocs []pending

	for oi, o := range objs {
		if o.Machine != out.Machine {
			return nil, fmt.Errorf("link: object %d has machine %d, expected %d", oi, o.Machine, out.Machine)
		}
		textBase := uint64(len(out.Text))
		dataBase := uint64(len(out.Data))
		out.Text = append(out.Text, o.Text...)
		out.Data = append(out.Data, o.Data...)

		// Place this object's defined symbols; collect local renames (a local name
		// that clashes with one already placed is uniquified per object).
		local := map[string]string{}
		for _, s := range o.Syms {
			base, ok := sectionBase(s.Section, textBase, dataBase)
			if !ok {
				continue // undefined, or a section this linker does not merge yet
			}
			rebased := s
			rebased.Value += base
			if s.Global {
				if _, dup := defined[s.Name]; dup {
					return nil, fmt.Errorf("link: duplicate symbol %q", s.Name)
				}
			} else if used[rebased.Name] {
				rebased.Name = fmt.Sprintf("%s.%d", s.Name, oi)
				local[s.Name] = rebased.Name
			}
			defined[rebased.Name] = rebased
			used[rebased.Name] = true
			out.Syms = append(out.Syms, rebased)
		}

		rebase := func(rs []obj.Reloc, base uint64, toData bool) {
			for _, r := range rs {
				r.Offset += base
				if n, ok := local[r.Sym]; ok {
					r.Sym = n
				}
				relocs = append(relocs, pending{r, toData})
			}
		}
		rebase(o.Relocs, textBase, false)
		rebase(o.DataRelocs, dataBase, true)
	}

	// Apply every intra-.text PC-relative relocation whose target is now defined;
	// keep the rest for the final link.
	for _, p := range relocs {
		if sym, ok := defined[p.r.Sym]; ok && !p.toData && sym.Section == obj.SecText && isPCRel(out.Machine, p.r.Type) {
			if err := patchBranch(out.Machine, out.Text, p.r, sym); err != nil {
				return nil, err
			}
			continue
		}
		if p.toData {
			out.DataRelocs = append(out.DataRelocs, p.r)
		} else {
			out.Relocs = append(out.Relocs, p.r)
		}
	}
	return out, nil
}

func sectionBase(k obj.SecKind, textBase, dataBase uint64) (uint64, bool) {
	switch k {
	case obj.SecText:
		return textBase, true
	case obj.SecData:
		return dataBase, true
	}
	return 0, false
}

// isPCRel reports whether a relocation type is an intra-.text PC-relative call
// or jump the linker can resolve now, for the given machine.
func isPCRel(machine uint16, typ uint32) bool {
	switch machine {
	case obj.EM_AARCH64:
		return typ == obj.R_AARCH64_CALL26 || typ == obj.R_AARCH64_JUMP26
	case obj.EM_X86_64:
		return typ == obj.R_X86_64_PC32 || typ == obj.R_X86_64_PLT32
	}
	return false
}

// patchBranch resolves a PC-relative relocation by writing the (now known)
// displacement into the branch instruction. This is link-time-final because both
// the branch and its target are in .text, so their relative distance does not
// depend on where .text is finally loaded.
func patchBranch(machine uint16, text []byte, r obj.Reloc, sym obj.Sym) error {
	switch machine {
	case obj.EM_AARCH64:
		return patchAArch64Branch(text, r, sym)
	case obj.EM_X86_64:
		return patchX86Rel32(text, r, sym)
	}
	return fmt.Errorf("link: cannot resolve relocation for machine %d", machine)
}

// patchAArch64Branch writes a CALL26/JUMP26 relocation into a branch's 26-bit
// immediate (the displacement in units of 4-byte instructions).
func patchAArch64Branch(text []byte, r obj.Reloc, sym obj.Sym) error {
	target := int64(sym.Value) + r.Addend
	disp := target - int64(r.Offset)
	if disp%4 != 0 {
		return fmt.Errorf("link: misaligned branch target for %q", r.Sym)
	}
	imm := disp / 4
	if imm < -(1<<25) || imm >= (1<<25) {
		return fmt.Errorf("link: branch to %q out of range (%d bytes)", r.Sym, disp)
	}
	word := binary.LittleEndian.Uint32(text[r.Offset:])
	word = (word & 0xfc000000) | uint32(imm)&0x03ffffff
	binary.LittleEndian.PutUint32(text[r.Offset:], word)
	return nil
}

// patchX86Rel32 writes a PC32/PLT32 relocation as a 32-bit displacement. The
// addend already accounts for the instruction-end bias (-4), so the value is
// simply sym + addend - place.
func patchX86Rel32(text []byte, r obj.Reloc, sym obj.Sym) error {
	disp := int64(sym.Value) + r.Addend - int64(r.Offset)
	if disp < math.MinInt32 || disp > math.MaxInt32 {
		return fmt.Errorf("link: call to %q out of range (%d bytes)", r.Sym, disp)
	}
	binary.LittleEndian.PutUint32(text[r.Offset:], uint32(int32(disp)))
	return nil
}
