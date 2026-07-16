package arm64

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/obj"
)

// Disassemble renders an object's .text as assembly text: one instruction per
// line, a label where each symbol begins, and symbol names in place of the
// fields the relocations stand for. It reads the machine code we emit, so tests
// and tools can check what was selected without a second, independently-written
// text emitter to keep in step with the encoder.
//
// The output is for reading, not for feeding back to an assembler: it names
// symbols without declaring them, and a relocated field is shown as the symbol
// rather than the zero actually encoded there.
func Disassemble(o *obj.Object) string {
	labels := textLabels(o)
	relocs := make(map[uint64]obj.Reloc, len(o.Relocs))
	for _, r := range o.Relocs {
		relocs[r.Offset] = r
	}

	var b strings.Builder
	for off := 0; off+4 <= len(o.Text); off += 4 {
		for _, name := range labels[uint64(off)] {
			fmt.Fprintf(&b, "%s:\n", name)
		}
		w := binary.LittleEndian.Uint32(o.Text[off:])
		line := a64.Disasm(w)
		if r, ok := relocs[uint64(off)]; ok {
			if s, ok := symbolic(r, w); ok {
				line = s
			}
		}
		fmt.Fprintf(&b, "\t%s\n", line)
	}
	return b.String()
}

// textLabels indexes the symbols defined in .text by their offset. Several
// symbols can share an offset, so each is a list, ordered for a stable result.
func textLabels(o *obj.Object) map[uint64][]string {
	labels := map[uint64][]string{}
	for _, s := range o.Syms {
		if s.Section == obj.SecText {
			labels[s.Value] = append(labels[s.Value], s.Name)
		}
	}
	for _, names := range labels {
		sort.Strings(names)
	}
	return labels
}

// symbolic renders the instruction at a relocation as the source would write it,
// with the symbol named in the field the linker will fill. It reports false for
// a relocation that does not belong to an instruction we know, leaving the plain
// decoding to stand.
func symbolic(r obj.Reloc, w uint32) (string, bool) {
	rd, rn := w&0x1f, w>>5&0x1f
	target := r.Sym
	if r.Addend != 0 {
		target = fmt.Sprintf("%s%+d", r.Sym, r.Addend)
	}
	switch r.Type {
	case obj.R_AARCH64_CALL26:
		return "bl " + target, true
	case obj.R_AARCH64_JUMP26:
		return "b " + target, true
	case obj.R_AARCH64_ADR_PREL_PG_HI21:
		return fmt.Sprintf("adrp x%d, %s", rd, target), true
	case obj.R_AARCH64_ADD_ABS_LO12_NC:
		return fmt.Sprintf("add x%d, x%d, #:lo12:%s", rd, rn, target), true

	case obj.R_AARCH64_TLSLE_ADD_TPREL_HI12:
		return fmt.Sprintf("add x%d, x%d, #:tprel_hi12:%s, lsl #12", rd, rn, target), true
	case obj.R_AARCH64_TLSLE_ADD_TPREL_LO12_NC:
		return fmt.Sprintf("add x%d, x%d, #:tprel_lo12_nc:%s", rd, rn, target), true

	case obj.R_AARCH64_TLSIE_ADR_GOTTPREL_PAGE21:
		return fmt.Sprintf("adrp x%d, :gottprel:%s", rd, target), true
	case obj.R_AARCH64_TLSIE_LD64_GOTTPREL_LO12_NC:
		return fmt.Sprintf("ldr x%d, [x%d, #:gottprel_lo12:%s]", rd, rn, target), true

	case obj.R_AARCH64_TLSGD_ADR_PAGE21:
		return fmt.Sprintf("adrp x%d, :tlsgd:%s", rd, target), true
	case obj.R_AARCH64_TLSGD_ADD_LO12_NC:
		return fmt.Sprintf("add x%d, x%d, #:tlsgd_lo12:%s", rd, rn, target), true
	}
	return "", false
}
