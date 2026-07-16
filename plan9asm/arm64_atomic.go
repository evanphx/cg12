package plan9asm

import (
	"fmt"
	"strings"
)

func (t *arm64Translator) translateAtomicLoad(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires an address and destination", instruction.Opcode)
	}
	width := atomicInstructionWidth(instruction.Opcode)
	destination, err := registerOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	address, err := t.memoryAddress(instruction.Operands[0], "")
	if err != nil {
		return err
	}
	mnemonic := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(instruction.Opcode, "W"), "B"))
	if strings.HasSuffix(instruction.Opcode, "B") {
		mnemonic += "b"
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, destination, address)
	return nil
}

func (t *arm64Translator) translateAtomicStore(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires a value and address", instruction.Opcode)
	}
	width := atomicInstructionWidth(instruction.Opcode)
	value, err := registerOperand(instruction.Operands[0], width)
	if err != nil {
		return err
	}
	address, err := t.memoryAddress(instruction.Operands[1], "")
	if err != nil {
		return err
	}
	mnemonic := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(instruction.Opcode, "W"), "B"))
	if strings.HasSuffix(instruction.Opcode, "B") {
		mnemonic += "b"
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s\n", mnemonic, value, address)
	return nil
}

func (t *arm64Translator) translateAtomicStoreExclusive(instruction *Instruction) error {
	if len(instruction.Operands) != 3 {
		return fmt.Errorf("%s requires a value, address, and status", instruction.Opcode)
	}
	width := atomicInstructionWidth(instruction.Opcode)
	value, err := registerOperand(instruction.Operands[0], width)
	if err != nil {
		return err
	}
	address, err := t.memoryAddress(instruction.Operands[1], "")
	if err != nil {
		return err
	}
	status, err := registerOperand(instruction.Operands[2], 32)
	if err != nil {
		return err
	}
	mnemonic := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(instruction.Opcode, "W"), "B"))
	if strings.HasSuffix(instruction.Opcode, "B") {
		mnemonic += "b"
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", mnemonic, status, value, address)
	return nil
}

func (t *arm64Translator) translateLSEAtomic(instruction *Instruction) error {
	if len(instruction.Operands) != 3 {
		return fmt.Errorf("%s requires a source, address, and destination", instruction.Opcode)
	}
	width := atomicInstructionWidth(instruction.Opcode)
	source, err := registerOperand(instruction.Operands[0], width)
	if err != nil {
		return err
	}
	address, err := t.memoryAddress(instruction.Operands[1], "")
	if err != nil {
		return err
	}
	destination, err := registerOperand(instruction.Operands[2], width)
	if err != nil {
		return err
	}
	if !t.lseEnabled {
		t.output.WriteString("\t.arch armv8.1-a+lse\n")
		t.lseEnabled = true
	}
	mnemonic := strings.ToLower(instruction.Opcode)
	suffix := mnemonic[len(mnemonic)-1]
	if suffix == 'w' || suffix == 'd' {
		mnemonic = mnemonic[:len(mnemonic)-1]
	}
	if strings.HasPrefix(mnemonic, "ldor") {
		mnemonic = "ldset" + strings.TrimPrefix(mnemonic, "ldor")
	}
	fmt.Fprintf(&t.output, "\t%s %s, %s, %s\n", mnemonic, source, destination, address)
	return nil
}

func (t *arm64Translator) translateBitwiseNot(instruction *Instruction) error {
	if len(instruction.Operands) != 2 {
		return fmt.Errorf("%s requires a source and destination", instruction.Opcode)
	}
	width := instructionWidth(instruction.Opcode)
	source, err := registerOperand(instruction.Operands[0], width)
	if err != nil {
		return err
	}
	destination, err := registerOperand(instruction.Operands[1], width)
	if err != nil {
		return err
	}
	fmt.Fprintf(&t.output, "\tmvn %s, %s\n", destination, source)
	return nil
}

func atomicInstructionWidth(opcode string) int {
	if strings.HasSuffix(opcode, "W") || strings.HasSuffix(opcode, "B") {
		return 32
	}
	return 64
}
