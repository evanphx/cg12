// Command interpbench times a bytecode interpreter: a switch over an opcode in a
// loop, which is the classic shape whose speed depends on where the dispatch
// lands. The program it runs is fixed, so the case does exactly the same work
// every time.
package main

import (
	"fmt"
	"time"
)

// rounds is how many timed rounds each case gets; the fastest is reported.
const rounds = 3

// sink keeps the control loop from being optimised away.
var sink uint64

// control is the fixed amount of integer arithmetic every case is divided by,
// so the machine's speed and its load divide out of the reported index. It is
// the same loop the crypto signing benchmark uses.
func control() {
	accumulator := uint64(1)
	for i := 0; i < 20_000_000; i++ {
		accumulator = accumulator*6364136223846793005 + 1442695040888963407
	}
	sink = accumulator
}

// measure returns the fastest of rounds timed rounds, in nanoseconds. Noise can
// only ever make a round slower, so the fastest is the least contaminated.
func measure(body func()) time.Duration {
	body()
	best := time.Duration(1<<63 - 1)
	for round := 0; round < rounds; round++ {
		start := time.Now()
		body()
		if elapsed := time.Since(start); elapsed < best {
			best = elapsed
		}
	}
	return best
}

func report(name string, body func()) {
	fmt.Printf("%s\t%d\n", name, int64(measure(body)))
}

const (
	opPush = iota
	opLoad
	opStore
	opAdd
	opSub
	opMul
	opXor
	opLess
	opJumpFalse
	opJump
	opHalt
)

type instruction struct {
	op      int
	operand int64
}

// execute runs a program over a small register file and an operand stack.
func execute(program []instruction, iterations int64) int64 {
	var stack [64]int64
	var registers [8]int64
	registers[0] = iterations
	top := 0
	pc := 0
	for {
		in := program[pc]
		pc++
		switch in.op {
		case opPush:
			stack[top] = in.operand
			top++
		case opLoad:
			stack[top] = registers[in.operand]
			top++
		case opStore:
			top--
			registers[in.operand] = stack[top]
		case opAdd:
			top--
			stack[top-1] += stack[top]
		case opSub:
			top--
			stack[top-1] -= stack[top]
		case opMul:
			top--
			stack[top-1] *= stack[top]
		case opXor:
			top--
			stack[top-1] ^= stack[top]
		case opLess:
			top--
			if stack[top-1] < stack[top] {
				stack[top-1] = 1
			} else {
				stack[top-1] = 0
			}
		case opJumpFalse:
			top--
			if stack[top] == 0 {
				pc = int(in.operand)
			}
		case opJump:
			pc = int(in.operand)
		case opHalt:
			return registers[1]
		}
	}
}

// counterLoop is `for i := 0; i < n; i++ { acc = acc*3 + i ^ acc }` in bytecode.
func counterLoop() []instruction {
	return []instruction{
		{opPush, 0}, {opStore, 2}, // i = 0
		{opPush, 1}, {opStore, 1}, // acc = 1
		// loop head at 4
		{opLoad, 2}, {opLoad, 0}, {opLess, 0}, {opJumpFalse, 21},
		{opLoad, 1}, {opPush, 3}, {opMul, 0}, {opLoad, 2}, {opAdd, 0},
		{opLoad, 1}, {opXor, 0}, {opStore, 1},
		{opLoad, 2}, {opPush, 1}, {opAdd, 0}, {opStore, 2},
		{opJump, 4},
		{opHalt, 0},
	}
}

func main() {
	report("control/spin-fixed-work", control)

	program := counterLoop()
	report("interp/bytecode-loop", func() {
		sink += uint64(execute(program, 800_000))
	})
}
