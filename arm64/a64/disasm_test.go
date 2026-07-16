package a64

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The disassembler is checked against the very pairs the assembler validates the
// encoder with, read the other way round: whatever text `as` agreed encodes to a
// word, Disasm must recover from that word. One set of facts, both directions.
func TestDisasmMatchesTheEncodedForm(t *testing.T) {
	for _, group := range [][]encodingCase{encodingCases, moreEncodingCases} {
		for _, c := range group {
			t.Run(c.asm, func(t *testing.T) {
				assert.Equal(t, c.asm, Disasm(c.word))
			})
		}
	}
}

// An instruction the decoder does not know must say so rather than invent a
// plausible one -- the same way the assembler reports a mnemonic it cannot encode.
func TestDisasmUnknownWordIsReported(t *testing.T) {
	assert.Equal(t, ".inst 0x00000000", Disasm(0))
	assert.Equal(t, ".inst 0xffffffff", Disasm(0xffffffff))
}
