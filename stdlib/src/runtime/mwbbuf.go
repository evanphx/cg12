// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This implements the write barrier buffer. The write barrier itself
// is gcWriteBarrier and is implemented in assembly.
//
// See mbarrier.go for algorithmic details on the write barrier. This
// file deals only with the buffer.
//
// The write barrier has a fast path and a slow path. The fast path
// simply enqueues to a per-P write barrier buffer. It's written in
// assembly and doesn't clobber any general purpose registers, so it
// doesn't have the usual overheads of a Go call.
//
// When the buffer fills up, the write barrier invokes the slow path
// (wbBufFlush) to flush the buffer to the GC work queues. In this
// path, since the compiler didn't spill registers, we spill *all*
// registers and disallow any GC safe points that could observe the
// stack frame (since we don't know the types of the spilled
// registers).

package runtime

import (
	"internal/goarch"
	"internal/runtime/atomic"
	"unsafe"
)

// testSmallBuf forces a small write barrier buffer to stress write
// barrier flushing.
const testSmallBuf = false

// wbBuf is a per-P buffer of pointers queued by the write barrier.
// This buffer is flushed to the GC workbufs when it fills up and on
// various GC transitions.
//
// This is closely related to a "sequential store buffer" (SSB),
// except that SSBs are usually used for maintaining remembered sets,
// while this is used for marking.
type wbBuf struct {
	// next points to the next slot in buf. It must not be a
	// pointer type because it can point past the end of buf and
	// must be updated without write barriers.
	//
	// This is a pointer rather than an index to optimize the
	// write barrier assembly.
	next uintptr

	// end points to just past the end of buf. It must not be a
	// pointer type because it points past the end of buf and must
	// be updated without write barriers.
	end uintptr

	// buf stores a series of pointers to execute write barriers on.
	buf [wbBufEntries]uintptr
}

const (
	// wbBufEntries is the maximum number of pointers that can be
	// stored in the write barrier buffer.
	//
	// This trades latency for throughput amortization. Higher
	// values amortize flushing overhead more, but increase the
	// latency of flushing. Higher values also increase the cache
	// footprint of the buffer.
	//
	// TODO: What is the latency cost of this? Tune this value.
	wbBufEntries = 512

	// Maximum number of entries that we need to ask from the
	// buffer in a single call.
	wbMaxEntriesPerCall = 8
)

// reset empties b by resetting its next and end pointers.
func (b *wbBuf) reset() {
	start := uintptr(unsafe.Pointer(&b.buf[0]))
	b.next = start
	if testSmallBuf {
		// For testing, make the buffer smaller but more than
		// 1 write barrier's worth, so it tests both the
		// immediate flush and delayed flush cases.
		b.end = uintptr(unsafe.Pointer(&b.buf[wbMaxEntriesPerCall+1]))
	} else {
		b.end = start + uintptr(len(b.buf))*unsafe.Sizeof(b.buf[0])
	}

	if (b.end-b.next)%unsafe.Sizeof(b.buf[0]) != 0 {
		throw("bad write barrier buffer bounds")
	}
}

// discard resets b's next pointer, but not its end pointer.
//
// This must be nosplit because it's called by wbBufFlush.
//
//go:nosplit
func (b *wbBuf) discard() {
	b.next = uintptr(unsafe.Pointer(&b.buf[0]))
}

// empty reports whether b contains no pointers.
func (b *wbBuf) empty() bool {
	return b.next == uintptr(unsafe.Pointer(&b.buf[0]))
}

// getX returns space in the write barrier buffer to store X pointers.
// getX will flush the buffer if necessary. Callers should use this as:
//
//	buf := &getg().m.p.ptr().wbBuf
//	p := buf.get2()
//	p[0], p[1] = old, new
//	... actual memory write ...
//
// The caller must ensure there are no preemption points during the
// above sequence. There must be no preemption points while buf is in
// use because it is a per-P resource. There must be no preemption
// points between the buffer put and the write to memory because this
// could allow a GC phase change, which could result in missed write
// barriers.
//
// getX must be nowritebarrierrec to because write barriers here would
// corrupt the write barrier buffer. It (and everything it calls, if
// it called anything) has to be nosplit to avoid scheduling on to a
// different P and a different buffer.
//
//go:nowritebarrierrec
//go:nosplit
func (b *wbBuf) get1() *[1]uintptr {
	if b.next+goarch.PtrSize > b.end {
		wbBufFlush()
	}
	p := (*[1]uintptr)(unsafe.Pointer(b.next))
	b.next += goarch.PtrSize
	return p
}

//go:nowritebarrierrec
//go:nosplit
func (b *wbBuf) get2() *[2]uintptr {
	if b.next+2*goarch.PtrSize > b.end {
		wbBufFlush()
	}
	p := (*[2]uintptr)(unsafe.Pointer(b.next))
	b.next += 2 * goarch.PtrSize
	return p
}

// wbBufFlush flushes the current P's write barrier buffer to the GC
// workbufs.
//
// This must not have write barriers because it is part of the write
// barrier implementation.
//
// This and everything it calls must be nosplit because 1) the stack
// contains untyped slots from gcWriteBarrier and 2) there must not be
// a GC safe point between the write barrier test in the caller and
// flushing the buffer.
//
// TODO: A "go:nosplitrec" annotation would be perfect for this.
//
//go:nowritebarrierrec
//go:nosplit
func wbBufFlush() {
	// Note: Every possible return from this function must reset
	// the buffer's next pointer to prevent buffer overflow.

	if getg().m.dying > 0 {
		// We're going down. Not much point in write barriers
		// and this way we can allow write barriers in the
		// panic path.
		getg().m.p.ptr().wbBuf.discard()
		return
	}

	// Switch to the system stack so we don't have to worry about
	// safe points.
	systemstack(func() {
		wbBufFlush1(getg().m.p.ptr())
	})
}

// wbBufFlush1 flushes p's write barrier buffer to the GC work queue.
//
// This must not have write barriers because it is part of the write
// barrier implementation, so this may lead to infinite loops or
// buffer corruption.
//
// This must be non-preemptible because it uses the P's workbuf.
//
//go:nowritebarrierrec
//go:systemstack
func wbBufFlush1(pp *p) {
	// Get the buffered pointers.
	start := uintptr(unsafe.Pointer(&pp.wbBuf.buf[0]))
	n := (pp.wbBuf.next - start) / unsafe.Sizeof(pp.wbBuf.buf[0])
	ptrs := pp.wbBuf.buf[:n]

	// Poison the buffer to make extra sure nothing is enqueued
	// while we're processing the buffer.
	pp.wbBuf.next = 0

	if useCheckmark {
		// Slow path for checkmark mode.
		for _, ptr := range ptrs {
			shade(ptr)
		}
		pp.wbBuf.reset()
		return
	}

	// Mark all of the pointers in the buffer and record only the
	// pointers we greyed. We use the buffer itself to temporarily
	// record greyed pointers.
	//
	// TODO: Should scanObject/scanblock just stuff pointers into
	// the wbBuf? Then this would become the sole greying path.
	//
	// TODO: We could avoid shading any of the "new" pointers in
	// the buffer if the stack has been shaded, or even avoid
	// putting them in the buffer at all (which would double its
	// capacity). This is slightly complicated with the buffer; we
	// could track whether any un-shaded goroutine has used the
	// buffer, or just track globally whether there are any
	// un-shaded stacks and flush after each stack scan.
	gcw := &pp.gcw
	pos := 0
	for _, ptr := range ptrs {
		if ptr < minLegalPointer {
			// nil pointers are very common, especially
			// for the "old" values. Filter out these and
			// other "obvious" non-heap pointers ASAP.
			//
			// TODO: Should we filter out nils in the fast
			// path to reduce the rate of flushes?
			continue
		}
		if tryDeferToSpanScan(ptr, gcw) {
			continue
		}
		obj, span, objIndex := findObject(ptr, 0, 0)
		if obj == 0 {
			continue
		}
		// TODO: Consider making two passes where the first
		// just prefetches the mark bits.
		mbits := span.markBitsForIndex(objIndex)
		if mbits.isMarked() {
			continue
		}
		mbits.setMarked()

		// Mark span.
		arena, pageIdx, pageMask := pageIndexOf(span.base())
		if arena.pageMarks[pageIdx]&pageMask == 0 {
			atomic.Or8(&arena.pageMarks[pageIdx], pageMask)
		}

		if span.spanclass.noscan() {
			gcw.bytesMarked += uint64(span.elemsize)
			continue
		}
		ptrs[pos] = obj
		pos++
	}

	// Enqueue the greyed objects.
	gcw.putObjBatch(ptrs[:pos])

	pp.wbBuf.reset()
}

// cg12WriteBarrierValueIsBad reports whether value is a word that
// wbBufFlush1's findObject call would reject: a pointer into a span that
// exists but is neither an in-use heap span nor a manually managed span.
// It follows exactly findObject's acceptance rule, so the diagnostic below
// fires on the same words the collector would later fault on.
//
//go:nosplit
func cg12WriteBarrierValueIsBad(value uintptr) bool {
	if value < minLegalPointer {
		return false
	}
	span := spanOf(value)
	if span == nil {
		return false
	}
	state := span.state.get()
	if state == mSpanInUse && value >= span.base() && value < span.limit {
		return false
	}
	if state == mSpanManual {
		return false
	}
	return true
}

// cg12AddressIsGoroutineStack reports whether address falls inside a span the
// stack allocator owns, which is how a live goroutine stack looks to spanOf.
//
//go:nosplit
func cg12AddressIsGoroutineStack(address uintptr) bool {
	if address < minLegalPointer {
		return false
	}
	span := spanOf(address)
	if span == nil {
		return false
	}
	if span.state.get() != mSpanManual {
		return false
	}
	return address >= span.base() && address < span.limit
}

// cg12AddressIsGlobal reports whether address falls inside a module's static
// data or bss. Go's memory model forbids such a word from ever holding a
// goroutine stack address, because nothing relocates it when the stack moves.
//
//go:nosplit
func cg12AddressIsGlobal(address uintptr) bool {
	for module := &firstmoduledata; module != nil; module = module.next {
		if address >= module.data && address < module.edata {
			return true
		}
		if address >= module.bss && address < module.ebss {
			return true
		}
		if address >= module.noptrdata && address < module.enoptrdata {
			return true
		}
		if address >= module.noptrbss && address < module.enoptrbss {
			return true
		}
	}
	return false
}

// cg12BadWriteBarrierWord records the write that cg12CheckWriteBarrierPair
// rejected, so the report can run on the system stack where there is room to
// print. Only one write is ever recorded, because the report throws.
var cg12BadWriteBarrierWord struct {
	slot     uintptr
	old      uintptr
	new      uintptr
	oldBad   bool
	newBad   bool
	stackNew bool
}

// cg12BulkBarrierRange records the destination, source and size of the bulk
// copy a barrier is running for, so a rejected word can be reported with the
// range it came from. Only the bulk paths in mbitmap.go set it.
var cg12BulkBarrierRange struct {
	dst   uintptr
	src   uintptr
	size  uintptr
	valid bool
}

// cg12CheckWriteBarrierPair validates the old and new words a pointer write
// barrier is about to buffer and reports the storing site when one of them
// would later make wbBufFlush1 throw "found bad pointer in Go heap". Throwing
// at the store rather than at the flush makes the traceback name the function
// that performed the write instead of the background marker that happened to
// drain the buffer. GODEBUG=cg12checkwb=1 enables it.
//
// cg12checkwb=2 additionally rejects a global data or bss word that receives a
// goroutine stack address. Nothing relocates a global when a stack moves, so
// such a word is the stale root that later fails the check above; catching the
// store makes the defect deterministic instead of racy.
//
// The frame must stay small: every caller is on a nosplit path.
//
//go:nosplit
func cg12CheckWriteBarrierPair(slot uintptr, old uintptr, new uintptr) {
	oldBad := cg12WriteBarrierValueIsBad(old)
	newBad := cg12WriteBarrierValueIsBad(new)
	stackNew := false
	if debug.cg12checkwb > 1 && cg12AddressIsGoroutineStack(new) && cg12AddressIsGlobal(slot) {
		stackNew = true
	}
	if !oldBad && !newBad && !stackNew {
		return
	}
	cg12BadWriteBarrierWord.slot = slot
	cg12BadWriteBarrierWord.old = old
	cg12BadWriteBarrierWord.new = new
	cg12BadWriteBarrierWord.oldBad = oldBad
	cg12BadWriteBarrierWord.newBad = newBad
	cg12BadWriteBarrierWord.stackNew = stackNew
	systemstack(cg12ReportBadWriteBarrierWord)
}

// cg12ReportBadWriteBarrierWord prints the recorded bad write and throws. It
// runs on the system stack so the printing does not need the nosplit reserve
// of the goroutine that performed the store.
func cg12ReportBadWriteBarrierWord() {
	record := &cg12BadWriteBarrierWord
	printlock()
	if record.stackNew {
		print("cg12checkwb: pointer write barrier stored a goroutine stack address into a global\n")
	} else {
		print("cg12checkwb: pointer write barrier buffered a word the collector will reject\n")
	}
	print("cg12checkwb: slot=", hex(record.slot), " old=", hex(record.old), " new=", hex(record.new))
	if record.oldBad {
		print(" bad=old")
	}
	if record.newBad {
		print(" bad=new")
	}
	if record.stackNew {
		print(" bad=new-is-stack")
	}
	print("\n")
	if cg12BulkBarrierRange.valid {
		print("cg12checkwb: bulk copy dst=", hex(cg12BulkBarrierRange.dst),
			" src=", hex(cg12BulkBarrierRange.src),
			" size=", cg12BulkBarrierRange.size, "\n")
		cg12DescribeWriteBarrierAddress("bulk-dst", cg12BulkBarrierRange.dst)
		cg12DescribeWriteBarrierAddress("bulk-src", cg12BulkBarrierRange.src)
		for offset := uintptr(0); offset < cg12BulkBarrierRange.size && offset < 256; offset += goarch.PtrSize {
			word := *(*uintptr)(unsafe.Pointer(cg12BulkBarrierRange.src + offset))
			print("cg12checkwb: src[", offset/goarch.PtrSize, "] = ", hex(word))
			if cg12WriteBarrierValueIsBad(word) {
				print("  <== bad")
			}
			print("\n")
		}
	}
	cg12DescribeWriteBarrierAddress("slot", record.slot)
	if record.oldBad {
		cg12DescribeWriteBarrierAddress("old", record.old)
	}
	if record.newBad || record.stackNew {
		cg12DescribeWriteBarrierAddress("new", record.new)
	}
	printunlock()
	getg().m.traceback = 2
	if record.stackNew {
		throw("cg12checkwb: global data word holds a goroutine stack address")
	}
	throw("cg12checkwb: pointer write barrier buffered a bad pointer")
}

// cg12CheckBulkBarrierWord is cg12CheckWriteBarrierPair for the bulk barrier
// paths, which copy a whole range rather than storing one word. It records the
// range with the rejected word so the report can show which element of the
// source is bad and what the rest of the source looks like.
//
//go:nosplit
func cg12CheckBulkBarrierWord(slot, old, new, dst, src, size uintptr) {
	if !cg12WriteBarrierValueIsBad(old) && !cg12WriteBarrierValueIsBad(new) {
		return
	}
	cg12BulkBarrierRange.dst = dst
	cg12BulkBarrierRange.src = src
	cg12BulkBarrierRange.size = size
	cg12BulkBarrierRange.valid = true
	cg12CheckWriteBarrierPair(slot, old, new)
}

// cg12DescribeWriteBarrierAddress prints what the runtime knows about one
// address: its span, that span's state, and, when the address is a live heap
// object, the object it falls inside.
func cg12DescribeWriteBarrierAddress(label string, address uintptr) {
	span := spanOf(address)
	if span == nil {
		print("cg12checkwb: ", label, " ", hex(address), " is in no span (global, off-heap or never mapped)\n")
		return
	}
	state := span.state.get()
	print("cg12checkwb: ", label, " ", hex(address),
		" span base=", hex(span.base()),
		" limit=", hex(span.limit),
		" state=", state,
		" elemsize=", span.elemsize,
		" offset=", hex(address-span.base()), "\n")
	if state != mSpanInUse || address < span.base() || address >= span.limit {
		return
	}
	base := span.base() + span.elemsize*((address-span.base())/span.elemsize)
	print("cg12checkwb: ", label, " object base=", hex(base),
		" size=", span.elemsize,
		" head=", hex(*(*uintptr)(unsafe.Pointer(base))), "\n")
}
