package ir

// SrcPos is a source-code position — the metadata that drives debug-info
// emission (a DWARF line-number program). File is a 1-based index into
// [Module.Files]; the zero value (File == 0) means "no position". Positions
// attach to instructions and blocks so a backend can map generated machine code
// back to source lines and columns.
type SrcPos struct {
	File uint32
	Line uint32
	Col  uint32
}

// Valid reports whether p names a source location.
func (p SrcPos) Valid() bool { return p.File != 0 && p.Line != 0 }

// File interns a source-file name into the module's file table, returning the
// 1-based index to store in a [SrcPos].
func (m *Module) File(name string) uint32 {
	for i, f := range m.Files {
		if f == name {
			return uint32(i + 1)
		}
	}
	m.Files = append(m.Files, name)
	return uint32(len(m.Files))
}

// FileName returns the source-file name for a 1-based index, or "" when the
// index is 0 or out of range.
func (m *Module) FileName(idx uint32) string {
	if idx == 0 || int(idx) > len(m.Files) {
		return ""
	}
	return m.Files[idx-1]
}
