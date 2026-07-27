// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

const (
	// vdsoArrayMax is the byte-size of a maximally sized array on this architecture.
	// See cmd/compile/internal/arm64/galign.go arch.MAXWIDTH initialization.
	vdsoArrayMax = 1<<50 - 1
)

// key and version at man 7 vdso : aarch64
var vdsoLinuxVersion = vdsoVersionKey{"LINUX_2.6.39", 0x75fcb89}

var vdsoSymbolKeys = []vdsoSymbolKey{
	{"__kernel_clock_gettime", 0xb0cd725, 0xdfa941fd, &vdsoClockgettimeSym},
	// cg12: the vDSO getrandom (__kernel_getrandom) is intentionally not looked
	// up. cg12's runtime does not yet fully support the vgetrandom fast path (its
	// arm64 assembly translation is incomplete), so on a kernel that exposes the
	// vDSO getrandom (Linux >= 6.11) vgetrandomInit would proceed and crash.
	// Leaving vdsoGetrandomSym zero makes vgetrandomInit return early and vgetrandom
	// report unsupported, so the runtime uses the getrandom syscall -- the same path
	// taken on older kernels that lack the vDSO entry.
}

var (
	vdsoClockgettimeSym uintptr
	vdsoGetrandomSym    uintptr
)
