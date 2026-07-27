package ir

import "sort"

// The Go frontend attaches package assembly (Plan 9 source, ABI0 signatures) to
// a module for the backend to translate. That payload is frontend-specific, so
// it rides the module's binary format as an opaque attachment blob rather than a
// typed section of the core unit format: EncodeAssembly/DecodeAssembly own its
// wire shape here, and MarshalBinary/DecodeModule only carry the bytes.

// asmAttachmentKey is the Module.Attachments key under which assembly is carried.
const asmAttachmentKey = "assembly"

// EncodeAssembly serializes assembly files to a self-contained, deterministic
// byte blob (maps are emitted in sorted key order so equal inputs encode equal).
func EncodeAssembly(files []AssemblyFile) []byte {
	e := &enc{}
	e.uv(uint64(len(files)))
	for _, assembly := range files {
		e.str(assembly.PackagePath)
		e.str(assembly.Path)
		e.str(assembly.Source)
		names := make([]string, 0, len(assembly.Defines))
		for name := range assembly.Defines {
			names = append(names, name)
		}
		sort.Strings(names)
		e.uv(uint64(len(names)))
		for _, name := range names {
			e.str(name)
			e.iv(assembly.Defines[name])
		}
		includeNames := make([]string, 0, len(assembly.Includes))
		for name := range assembly.Includes {
			includeNames = append(includeNames, name)
		}
		sort.Strings(includeNames)
		e.uv(uint64(len(includeNames)))
		for _, name := range includeNames {
			e.str(name)
			e.str(assembly.Includes[name])
		}
		e.encAssemblyFloatSlots(assembly.FloatInputs)
		e.encAssemblyFloatSlots(assembly.FloatOutputs)
		e.encAssemblySignatures(assembly.Signatures)
	}
	return e.buf
}

// DecodeAssembly reverses EncodeAssembly.
func DecodeAssembly(data []byte) ([]AssemblyFile, error) {
	if len(data) == 0 {
		return nil, nil
	}
	d := &dec{buf: data}
	files := make([]AssemblyFile, int(d.uv()))
	for i := range files {
		files[i].PackagePath = d.str()
		files[i].Path = d.str()
		files[i].Source = d.str()
		defineCount := int(d.uv())
		if defineCount != 0 {
			files[i].Defines = make(map[string]int64, defineCount)
			for range defineCount {
				files[i].Defines[d.str()] = d.iv()
			}
		}
		includeCount := int(d.uv())
		if includeCount != 0 {
			files[i].Includes = make(map[string]string, includeCount)
			for range includeCount {
				files[i].Includes[d.str()] = d.str()
			}
		}
		files[i].FloatInputs = d.decAssemblyFloatSlots()
		files[i].FloatOutputs = d.decAssemblyFloatSlots()
		files[i].Signatures = d.decAssemblySignatures()
	}
	return files, d.err
}

func (e *enc) encAssemblyFloatSlots(slots map[string][]int) {
	names := make([]string, 0, len(slots))
	for name := range slots {
		names = append(names, name)
	}
	sort.Strings(names)
	e.uv(uint64(len(names)))
	for _, name := range names {
		e.str(name)
		offsets := slots[name]
		e.uv(uint64(len(offsets)))
		for _, offset := range offsets {
			e.iv(int64(offset))
		}
	}
}

func (e *enc) encAssemblySignatures(signatures map[string]AsmSignature) {
	names := make([]string, 0, len(signatures))
	for name := range signatures {
		names = append(names, name)
	}
	sort.Strings(names)
	e.uv(uint64(len(names)))
	for _, name := range names {
		e.str(name)
		signature := signatures[name]
		e.encAssemblySlots(signature.Params)
		e.encAssemblySlots(signature.Results)
	}
}

func (e *enc) encAssemblySlots(slots []AsmSlot) {
	e.uv(uint64(len(slots)))
	for _, slot := range slots {
		e.str(slot.Name)
		e.iv(int64(slot.Offset))
		e.u8(byte(slot.Cls))
		e.uv(uint64(slot.Width))
		e.boolean(slot.GCRef)
		e.iv(int64(slot.Group))
	}
}

func (d *dec) decAssemblyFloatSlots() map[string][]int {
	count := int(d.uv())
	if count == 0 {
		return nil
	}
	slots := make(map[string][]int, count)
	for range count {
		name := d.str()
		offsets := make([]int, int(d.uv()))
		for index := range offsets {
			offsets[index] = int(d.iv())
		}
		slots[name] = offsets
	}
	return slots
}

func (d *dec) decAssemblySignatures() map[string]AsmSignature {
	count := int(d.uv())
	if count == 0 {
		return nil
	}
	signatures := make(map[string]AsmSignature, count)
	for range count {
		name := d.str()
		signatures[name] = AsmSignature{
			Params:  d.decAssemblySlots(),
			Results: d.decAssemblySlots(),
		}
	}
	return signatures
}

func (d *dec) decAssemblySlots() []AsmSlot {
	slots := make([]AsmSlot, int(d.uv()))
	for index := range slots {
		slots[index] = AsmSlot{
			Name:   d.str(),
			Offset: int(d.iv()),
			Cls:    Cls(d.u8()),
			Width:  int(d.uv()),
			GCRef:  d.boolean(),
			Group:  int(d.iv()),
		}
	}
	return slots
}
