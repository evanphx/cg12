package interp

const pageSize = 4096

// Fixed address-space segments. Bases are arbitrary but stable, with gaps large
// enough that segments never share a page. The null page is never mapped, so a
// null dereference faults.
const (
	segText   = 0x0000_1000 // 16-byte descriptors per function/extern/block; not load/store-able
	segGlobal = 0x0010_0000 // module ir.Data, in order
	segCommon = 0x2000_0000 // DefineExtern-created objects (a C driver's `int a;`)
	segHeap   = 0x4000_0000 // malloc/calloc bump allocator
	segStack  = 0x7000_0000 // grows downward
)

// memory is a sparse, byte-addressed, little-endian address space. Pages are
// created on demand; an access to an unmapped page faults (returns ok=false).
// Alignment is not enforced — AArch64 permits unaligned scalar access and QBE
// programs rely on it.
type memory struct {
	pages map[uint64]*[pageSize]byte
}

func newMemory() *memory { return &memory{pages: map[uint64]*[pageSize]byte{}} }

func (m *memory) page(addr uint64, create bool) (*[pageSize]byte, uint64) {
	idx := addr &^ (pageSize - 1)
	if p := m.pages[idx]; p != nil {
		return p, addr - idx
	}
	if !create {
		return nil, 0
	}
	p := new([pageSize]byte)
	m.pages[idx] = p
	return p, addr - idx
}

// mapRange ensures every page spanned by [addr, addr+size) exists.
func (m *memory) mapRange(addr, size uint64) {
	if size == 0 {
		m.page(addr, true)
		return
	}
	for a := addr &^ (pageSize - 1); a < addr+size; a += pageSize {
		m.page(a, true)
	}
}

// load reads size (1..8) bytes little-endian; ok=false if any byte is unmapped.
func (m *memory) load(addr uint64, size int) (uint64, bool) {
	var v uint64
	for i := 0; i < size; i++ {
		p, off := m.page(addr+uint64(i), false)
		if p == nil {
			return 0, false
		}
		v |= uint64(p[off]) << (8 * i)
	}
	return v, true
}

// store writes the low size bytes of bits little-endian; ok=false if unmapped.
func (m *memory) store(addr uint64, size int, bits uint64) bool {
	for i := 0; i < size; i++ {
		p, off := m.page(addr+uint64(i), false)
		if p == nil {
			return false
		}
		p[off] = byte(bits >> (8 * i))
	}
	return true
}

func (m *memory) readBytes(addr uint64, n int) ([]byte, bool) {
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		p, off := m.page(addr+uint64(i), false)
		if p == nil {
			return nil, false
		}
		out[i] = p[off]
	}
	return out, true
}

func (m *memory) writeBytes(addr uint64, data []byte) bool {
	for i, b := range data {
		p, off := m.page(addr+uint64(i), false)
		if p == nil {
			return false
		}
		p[off] = b
	}
	return true
}
