package plan9asm

import (
	"fmt"
	"strings"
)

func (t *arm64Translator) translateCRC32(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires a source and accumulator", instruction.Opcode)
	}
	if !t.crcEnabled {
		t.output.WriteString("\t.arch_extension crc\n")
		t.crcEnabled = true
	}

	dataWidth := 32
	if strings.HasSuffix(instruction.Opcode, "X") {
		dataWidth = 64
	}
	source, err := registerOperand(instruction.Operands[0], dataWidth)
	if err != nil {
		return err
	}
	accumulator, err := registerOperand(instruction.Operands[1], 32)
	if err != nil {
		return err
	}
	fmt.Fprintf(
		&t.output,
		"\t%s %s, %s, %s\n",
		strings.ToLower(instruction.Opcode),
		accumulator,
		accumulator,
		source,
	)
	return nil
}
