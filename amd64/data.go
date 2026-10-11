package amd64

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/evanphx/cg12/internal/gometa"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// addData appends a data definition to the object, emitting an ABS64 .rela.data
// relocation for any pointer-to-symbol item.
func addData(o *obj.Object, d *ir.Data) error {
	return addDataWithBSSAndFixups(o, d, true, nil)
}

// relativeDataFixup is a symbol difference a Go datum stores (the runtime's
// type and name offsets), resolved once every symbol in the object is placed.
type relativeDataFixup struct {
	section    obj.SecKind
	offset     uint64
	target     string
	relativeTo string
	addend     int64
	size       int
}

// addDataWithBSSAndFixups appends one data definition. allowBSS lets an
// all-zero datum go to .bss; a Go module keeps its globals in one contiguous
// data range instead, because the runtime's moduledata and GC program describe
// that range. fixups collects symbol differences to resolve after layout; nil
// resolves them immediately, which requires both symbols to be placed already.
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
				value, err := relativeDataValue(o, sanitize(it.Sym), sanitize(it.RelativeTo), it.Off)
				if err != nil {
					return fmt.Errorf("amd64: %w", err)
				}
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
			if sec == obj.SecTdata {
				return fmt.Errorf("amd64: thread-local %q cannot hold the address of %q: every thread's copy would need its own relocation", d.Name, it.Sym)
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
			relocationType := uint32(obj.R_X86_64_64)
			relocationSize := 8
			if it.Sub.Size() == 4 {
				relocationType = obj.R_X86_64_32
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
	o.Syms = append(o.Syms, obj.Sym{
		Name: sanitize(d.Name), Section: sec, Value: base,
		Size: uint64(len(*buf)) - base, Global: d.Linkage.Export,
		TLS: sec == obj.SecTdata,
	})
	return nil
}

// relativeDataValue is target - relativeTo + addend for two symbols already
// placed in the same section of the object.
func relativeDataValue(o *obj.Object, target, relativeTo string, addend int64) (int64, error) {
	targetSymbol, targetFound := gometa.ObjectSymbol(o, target)
	if !targetFound {
		return 0, fmt.Errorf("relative data reference %q is undefined", target)
	}
	baseSymbol, baseFound := gometa.ObjectSymbol(o, relativeTo)
	if !baseFound {
		return 0, fmt.Errorf("relative data base %q is undefined", relativeTo)
	}
	if targetSymbol.Section != baseSymbol.Section {
		return 0, fmt.Errorf("relative data reference %q and base %q are in different sections", target, relativeTo)
	}
	return int64(targetSymbol.Value) - int64(baseSymbol.Value) + addend, nil
}

// resolveRelativeDataFixups writes every deferred symbol difference.
func resolveRelativeDataFixups(o *obj.Object, fixups []relativeDataFixup) error {
	for _, fixup := range fixups {
		value, err := relativeDataValue(o, fixup.target, fixup.relativeTo, fixup.addend)
		if err != nil {
			return err
		}
		buffer, ok := objectSectionData(o, fixup.section)
		if !ok {
			return fmt.Errorf("unsupported destination section %d", fixup.section)
		}
		if fixup.offset+uint64(fixup.size) > uint64(len(buffer)) {
			return fmt.Errorf("destination for %q lies outside section", fixup.target)
		}
		copy(buffer[fixup.offset:], appendInt(nil, value, fixup.size))
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

// sanitize spells a symbol name for the object; it is the canonical linker
// mangling shared with the frontend-agnostic passes (see ir.LinkerSymbol).
func sanitize(name string) string { return ir.LinkerSymbol(name) }

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

// rodataSection is the ir.Linkage.Section a front end marks read-only data with.
const rodataSection = ".rodata"
