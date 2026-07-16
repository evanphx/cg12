package amd64

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// addData appends a data definition to the object, emitting an ABS64 .rela.data
// relocation for any pointer-to-symbol item.
func addData(o *obj.Object, d *ir.Data) error {
	// A thread-local's bytes are not data used in place: they are the image every
	// new thread's block is initialized from. They go to .tdata, and the symbol
	// records an offset within that block rather than an address.
	buf, sec := &o.Data, obj.SecData
	if d.Linkage.Thread {
		buf, sec = &o.Tdata, obj.SecTdata
		if d.Align > o.TlsAlign {
			o.TlsAlign = d.Align
		}
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
		case it.Sym != "":
			if sec == obj.SecTdata {
				return fmt.Errorf("amd64: thread-local %q cannot hold the address of %q: every thread's copy would need its own relocation", d.Name, it.Sym)
			}
			o.DataRelocs = append(o.DataRelocs, obj.Reloc{
				Offset: uint64(len(*buf)), Sym: sanitize(it.Sym),
				Type: obj.R_X86_64_64, Addend: it.Off,
			})
			*buf = append(*buf, make([]byte, 8)...)
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

// sanitize turns an IR name into a valid symbol-name component.
func sanitize(name string) string {
	var sb strings.Builder
	for _, r := range name {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	if sb.Len() == 0 {
		return "anon"
	}
	return sb.String()
}
