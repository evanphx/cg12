package arm64

import (
	"fmt"
	"os"
	"strconv"
)

// Code placement in .text.
//
// The emitter lays every function down at whatever offset the previous one
// happened to end at, so a hot loop's position inside the processor's
// instruction-fetch granule is a running total of every byte emitted before it
// anywhere in the module. That makes a benchmark's number depend on the size of
// unrelated code: a commit that changes a cold branch's length by 16 bytes moves
// every function after it and can swing a measurement by several percent without
// changing one instruction of the code being measured.
//
// textLayout is the policy that decides how much padding, if any, to spend on
// making a function's -- or a loop header's -- address independent of what came
// before it. The zero value is the historical behaviour: no alignment at all.
type textLayout struct {
	// funcAlign is the boundary each function entry is placed on. 0 or 4 means
	// no alignment beyond the instruction size.
	funcAlign int

	// loopFuncsOnly restricts funcAlign to functions that contain a backward
	// branch. Straight-line code is fetched once; a loop is fetched every
	// iteration, so it is where the granule boundary is paid for repeatedly.
	loopFuncsOnly bool

	// loopAlign is the boundary a loop header block is placed on inside its
	// function. It only means anything when the function entry is aligned at
	// least as well, because an offset within a function is an address only
	// relative to where the function landed.
	loopAlign int

	// textPad puts this many bytes of padding in front of the first function,
	// shifting the whole module's code. It generates nothing anyone would ship;
	// it exists so a placement experiment can hold a program byte-identical and
	// move only its address.
	textPad int
}

// layout is the placement policy every entry point into this backend uses.
//
// It is package state rather than a field of Options because it is a property of
// the code the backend emits, not of one call: the runtime pack, the program
// module and every prebuilt object have to agree about it, and they are compiled
// through four different exported functions.
var layout = layoutFromEnvironment()

// alignFor is the boundary a function with (or without) a loop is placed on.
func (l textLayout) alignFor(hasLoop bool) int {
	if l.funcAlign <= 4 {
		return 0
	}
	if l.loopFuncsOnly && !hasLoop {
		return 0
	}
	return l.funcAlign
}

// textAlign is the alignment the .text section itself must be given for this
// policy to mean anything in the linked image. Padding a function to 32 inside an
// object whose section the linker may place on any 4-byte boundary aligns it
// relative to nothing.
func (l textLayout) textAlign() int {
	align := 4
	if l.funcAlign > align {
		align = l.funcAlign
	}
	if l.loopAlign > align {
		align = l.loopAlign
	}
	return align
}

// identity is a short string naming this policy, for a build cache key. A cached
// artifact built under one policy is wrong under another, and the layout is not
// visible in the compiler binary's bytes.
func (l textLayout) identity() string {
	return fmt.Sprintf("funcalign=%d;looponly=%v;loopalign=%d;textpad=%d",
		l.funcAlign, l.loopFuncsOnly, l.loopAlign, l.textPad)
}

// TextLayoutIdentity names the placement policy this backend is compiled to
// emit, for callers that cache compiled artifacts.
func TextLayoutIdentity() string { return layout.identity() }

// The shipped placement policy: a 32-byte entry for a function that contains a
// backward branch, and nothing for one that does not.
//
// 32 because that is the instruction fetch granule on the Neoverse-N1 this was
// measured on, and because absorbing an upstream size change requires an
// alignment at least as large as the granule -- 16 halves the number of
// placements a program can land in and leaves the phase flipping between 0 and
// 16, which is not a fix.
//
// Only functions with a loop because that is where the difference is: a
// straight-line function is fetched once, and restricting the padding to
// looping functions costs 0.72% of .text against 2.37% for every function, for
// the same measured result. See CCWORK_REPORT.md, "Should goc align function
// entries", for the corpus this comes from: across 19 timed cases in eight
// programs, the spread a case's elapsed time has when only its address moves
// falls from a median of 14.6% to 1.0%, against a 0.08% measurement floor, and
// the mean cost falls 2.95%.
//
// defaultLoopAlign is 0 because aligning loop *headers* as well measured no
// better than not doing so and costs six times as much code: a function has one
// entry and Go's range loops and bounds-check re-entry give it several
// back-edge targets, so there are more loop heads in a program than functions.
const (
	defaultFuncAlign         = 32
	defaultLoopFunctionsOnly = true
	defaultLoopAlign         = 0
)

// layoutFromEnvironment reads the placement policy, with the shipped defaults
// below and an environment override for measuring alternatives.
//
// The override is deliberate: deciding what to align is a question about a corpus
// of programs, and answering it means building the same corpus several ways. A
// value that is not a power of two, or not a multiple of the 4-byte instruction
// size, is ignored rather than honoured badly.
func layoutFromEnvironment() textLayout {
	l := textLayout{
		funcAlign:     defaultFuncAlign,
		loopFuncsOnly: defaultLoopFunctionsOnly,
		loopAlign:     defaultLoopAlign,
	}
	if v, ok := alignmentFromEnvironment("GOC_FUNC_ALIGN"); ok {
		l.funcAlign = v
	}
	if v, ok := alignmentFromEnvironment("GOC_LOOP_ALIGN"); ok {
		l.loopAlign = v
	}
	if v := os.Getenv("GOC_ALIGN_LOOP_FUNCS_ONLY"); v != "" {
		l.loopFuncsOnly = v != "0"
	}
	if v, err := strconv.Atoi(os.Getenv("GOC_TEXT_PAD")); err == nil && v >= 0 && v%4 == 0 {
		l.textPad = v
	}
	return l
}

func alignmentFromEnvironment(name string) (int, bool) {
	v, err := strconv.Atoi(os.Getenv(name))
	if err != nil || v < 0 || v > 4096 {
		return 0, false
	}
	if v != 0 && (v%4 != 0 || v&(v-1) != 0) {
		return 0, false
	}
	return v, true
}

// alignText pads code with no-ops up to the next multiple of align.
func alignText(code []byte, align int) []byte {
	if align <= 4 {
		return code
	}
	for len(code)%align != 0 {
		code = append(code, alignmentNop...)
	}
	return code
}

// alignmentNop is `nop` (a HINT), little-endian. Padding between functions is
// never executed, but a no-op keeps a disassembly of .text readable and is what
// every other toolchain leaves there; a64's decoder already names it.
var alignmentNop = []byte{0x1f, 0x20, 0x03, 0xd5}
