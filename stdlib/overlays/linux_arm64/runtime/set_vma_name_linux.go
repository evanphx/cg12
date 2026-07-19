// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && arm64

package runtime

import "unsafe"

// Anonymous VMA names are diagnostic decoration. Leave them disabled while
// the managed AAPCS64 stack-argument area is brought up.
func setVMANameSupported() bool {
	return false
}

func setVMAName(_ unsafe.Pointer, _ uintptr, _ string) {
}
