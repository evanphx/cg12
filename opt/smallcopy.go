package opt

import (
	"github.com/evanphx/cg12/ir"
)

// ExpandSmallCopies rewrites a memcpy, memmove or memset of a small constant
// size into the loads and stores it stands for.
//
// cg12 emitted a library call for every one of these, however small. Compiling
// SQLite it produced 609 calls with a constant size, 450 of them 64 bytes or
// under, including 170 that copied four bytes and 64 that copied eight -- three
// instructions of argument setup and a call into libc to move one word between
// two adjacent stack slots. gcc, on the same source, emits 24 constant-size calls
// and the smallest is 296 bytes: everything below that it expands inline. That
// gap showed up in a profile as libc's own memcpy running 1.5x hotter in the cg12
// build than in the gcc one, on identical library code.
//
// Expanding also opens the door for the passes that follow, which turned out to
// matter more than the calls themselves. A copy is an opaque external call, so
// anything whose address reaches one has escaped and must stay in memory; once it
// is a store, nothing has escaped and the aggregate decomposes into registers
// like any other local. testdata/escape/array_small_memset.c is that case, and it
// stopped needing a stack slot at all. That is why this runs before the second
// mem2reg rather than at the end of the pipeline.
//
// On the direct cost, though, be modest. Removing all 450 calls from SQLite moved
// about 29ms of a 6200ms run -- half a percent, under what scripts/bench-c.sh
// can resolve, and the benchmark duly reported no change. Lua has only 13 such
// sites and did not move at all. The case for this is that it is plainly right,
// costs no compile time (2.92s -> 2.80s on SQLite) and no code size (-0.4%), and
// unblocks decomposition; it is not that it made anything measurably faster.
//
// What is not expanded: a size that is not constant, a size above
// smallCopyLimit, and a memset whose fill byte is not constant. The size is a
// constant here more often than it looks in the front end's output, where
// memcpy(a, b, 8) arrives as a call whose length operand is a temp holding
// `extsw 8`; fold has run by this point and turned it into the literal.
//
// These calls all define a result -- memcpy returns its destination -- even
// though C code effectively never reads it. The expansion therefore ends with a
// copy of the destination into that temp, which DCE removes when, as usual,
// nothing wanted it.
func ExpandSmallCopies(f *ir.Func) bool {
	changed := false
	for _, blk := range f.Blocks {
		var out []ir.Instr
		rewrote := false
		for i := range blk.Instrs {
			in := blk.Instrs[i]
			if expandCopy(f, in, &out) {
				rewrote = true
				continue
			}
			out = append(out, in)
		}
		if rewrote {
			blk.Instrs = out
			changed = true
		}
	}
	return changed
}

// smallCopyLimit is the largest copy expanded inline, in bytes.
//
// The chunks here are eight bytes, so N bytes cost 2*ceil(N/8) instructions. At
// 64 that is sixteen, against four of argument setup, a call, and whatever libc
// spends getting started -- inline is clearly ahead. By 128 it is thirty-two and
// roughly a wash, and beyond that libc's bulk loop wins. gcc carries a much
// higher threshold because it copies sixteen bytes at a time through SIMD
// registers; teaching this to use ClsQ would earn the same right, and is the
// obvious next step if these ever show up hot.
//
// Raising it to 128 was measured: it converts about a hundred more calls, almost
// all of them memset, for 0.25% more text. On SQLite that is not worth it --
// memset does not appear in the profile at all -- so the limit stays where the
// instruction count still clearly favours inlining.
const smallCopyLimit = 64

// expandCopy appends the expansion of in to out and reports whether it did. It
// leaves in alone, and reports false, for anything it does not handle.
func expandCopy(f *ir.Func, in ir.Instr, out *[]ir.Instr) bool {
	if in.Op != ir.OCall || len(in.Args) != 4 {
		return false
	}
	name := constSymbolName(f, in.Arg(0))
	size, known := constInt(f, in.Arg(3))
	if !known || size < 0 || size > smallCopyLimit {
		return false
	}
	dst := in.Arg(1)
	switch name {
	case "memcpy", "goc_memcpy":
		// C forbids overlap, so each chunk may be stored as soon as it is loaded.
		// Interleaving keeps only one value live at a time, which matters: register
		// pressure is what this compiler is worst at.
		aggCopy(f, dst, in.Arg(2), int(size), out)
	case "memmove", "goc_memmove":
		// memmove must work when the regions overlap, so every chunk is loaded
		// before any is stored. That holds one value live per chunk, which is why
		// it is worth keeping separate from the memcpy case rather than using this
		// shape for both.
		moveCopy(f, dst, in.Arg(2), int(size), out)
	case "memset", "goc_memset":
		fill, knownFill := constInt(f, in.Arg(2))
		if !knownFill {
			return false
		}
		setCopy(f, dst, byte(fill), int(size), out)
	default:
		return false
	}
	// These functions return their destination. Nothing normally reads it, but the
	// call defined a temp and anything still referring to it must keep working.
	if !in.To.IsNone() {
		*out = append(*out, ir.Instr{Op: ir.OCopy, Cls: in.Cls, To: in.To, Args: []ir.Ref{dst}})
	}
	return true
}

// moveCopy copies size bytes from src to dst, loading every chunk before storing
// any, so it is correct when the two regions overlap.
func moveCopy(f *ir.Func, dst, src ir.Ref, size int, out *[]ir.Instr) {
	type chunk struct {
		val     ir.Ref
		off     int
		storeOp ir.Op
		cls     ir.Cls
	}
	var chunks []chunk
	off := 0
	for _, w := range []struct {
		bytes           int
		loadOp, storeOp ir.Op
		cls             ir.Cls
	}{
		{8, ir.OLoadl, ir.OStorel, ir.ClsL},
		{4, ir.OLoaduw, ir.OStorew, ir.ClsW},
		{2, ir.OLoaduh, ir.OStoreh, ir.ClsW},
		{1, ir.OLoadub, ir.OStoreb, ir.ClsW},
	} {
		for size-off >= w.bytes {
			s := aggOffset(f, src, off, out)
			v := f.NewTemp("", w.cls)
			*out = append(*out, ir.Instr{Op: w.loadOp, Cls: w.cls, To: v, Args: []ir.Ref{s}})
			chunks = append(chunks, chunk{v, off, w.storeOp, w.cls})
			off += w.bytes
		}
	}
	for _, c := range chunks {
		d := aggOffset(f, dst, c.off, out)
		*out = append(*out, ir.Instr{Op: c.storeOp, Cls: c.cls, Args: []ir.Ref{c.val, d}})
	}
}

// setCopy writes size bytes of fill to dst. The byte is splatted across each
// chunk's width, so a 64-byte zero fill is eight stores of a single constant
// rather than sixty-four byte stores.
func setCopy(f *ir.Func, dst ir.Ref, fill byte, size int, out *[]ir.Instr) {
	splat := func(width int) int64 {
		var v uint64
		for i := 0; i < width; i++ {
			v |= uint64(fill) << (8 * i)
		}
		return int64(v)
	}
	off := 0
	chunk := func(w int, storeOp ir.Op, cls ir.Cls) {
		for size-off >= w {
			d := aggOffset(f, dst, off, out)
			var val ir.Ref
			if cls == ir.ClsL {
				val = f.Long(splat(w))
			} else {
				val = f.Word(splat(w))
			}
			*out = append(*out, ir.Instr{Op: storeOp, Cls: cls, Args: []ir.Ref{val, d}})
			off += w
		}
	}
	chunk(8, ir.OStorel, ir.ClsL)
	chunk(4, ir.OStorew, ir.ClsW)
	chunk(2, ir.OStoreh, ir.ClsW)
	chunk(1, ir.OStoreb, ir.ClsW)
}
