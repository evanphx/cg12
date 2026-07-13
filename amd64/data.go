package amd64

import (
	"encoding/binary"
	"math"
	"strings"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// addData appends a data definition to the object, emitting an ABS64 .rela.data
// relocation for any pointer-to-symbol item.
func addData(o *obj.Object, d *ir.Data) {
	if d.Align > 0 {
		for len(o.Data)%d.Align != 0 {
			o.Data = append(o.Data, 0)
		}
	}
	base := uint64(len(o.Data))
	for _, it := range d.Items {
		switch {
		case it.Zero > 0:
			o.Data = append(o.Data, make([]byte, it.Zero)...)
		case it.Str != "":
			o.Data = append(o.Data, []byte(it.Str)...)
		case it.Sym != "":
			o.DataRelocs = append(o.DataRelocs, obj.Reloc{
				Offset: uint64(len(o.Data)), Sym: sanitize(it.Sym),
				Type: obj.R_X86_64_64, Addend: it.Off,
			})
			o.Data = append(o.Data, make([]byte, 8)...)
		case len(it.Flts) > 0:
			for _, v := range it.Flts {
				o.Data = appendInt(o.Data, floatBitsOf(it.Sub, v), it.Sub.Size())
			}
		default:
			for _, v := range it.Ints {
				o.Data = appendInt(o.Data, v, it.Sub.Size())
			}
		}
	}
	o.Syms = append(o.Syms, obj.Sym{
		Name: sanitize(d.Name), Section: obj.SecData, Value: base,
		Size: uint64(len(o.Data)) - base, Global: d.Linkage.Export,
	})
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
