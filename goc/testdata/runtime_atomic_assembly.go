package main

import (
	"internal/cpu"
	"internal/runtime/atomic"
)

func exerciseAtomics() {
	var word uint32
	atomic.StoreRel(&word, 10)
	if atomic.LoadAcq(&word) != 10 {
		panic("atomic 32-bit load or store failed")
	}
	if !atomic.CasRel(&word, 10, 20) || atomic.Cas(&word, 10, 30) {
		panic("atomic 32-bit compare-and-swap failed")
	}
	if atomic.Xadd(&word, 5) != 25 {
		panic("atomic 32-bit add failed")
	}
	if atomic.Xchg(&word, 7) != 25 || word != 7 {
		panic("atomic 32-bit exchange failed")
	}
	if atomic.Or32(&word, 8) != 7 || word != 15 {
		panic("atomic 32-bit OR failed")
	}
	if atomic.And32(&word, 6) != 15 || word != 6 {
		panic("atomic 32-bit AND failed")
	}

	var wide uint64
	atomic.StoreRel64(&wide, 100)
	if atomic.LoadAcq64(&wide) != 100 {
		panic("atomic 64-bit load or store failed")
	}
	if !atomic.Cas64(&wide, 100, 200) {
		panic("atomic 64-bit compare-and-swap failed")
	}
	if atomic.Xadd64(&wide, 25) != 225 {
		panic("atomic 64-bit add failed")
	}
	if atomic.Xchg64(&wide, 9) != 225 || wide != 9 {
		panic("atomic 64-bit exchange failed")
	}
	if atomic.Or64(&wide, 6) != 9 || wide != 15 {
		panic("atomic 64-bit OR failed")
	}
	if atomic.And64(&wide, 10) != 15 || wide != 10 {
		panic("atomic 64-bit AND failed")
	}

	var small uint8
	atomic.Store8(&small, 0x30)
	if atomic.Load8(&small) != 0x30 {
		panic("atomic byte load or store failed")
	}
	if atomic.Xchg8(&small, 0x0f) != 0x30 {
		panic("atomic byte exchange failed")
	}
	atomic.Or8(&small, 0x30)
	atomic.And8(&small, 0x3c)
	if small != 0x3c {
		panic("atomic byte operation failed")
	}
}

func main() {
	hasAtomics := cpu.ARM64.HasATOMICS
	cpu.ARM64.HasATOMICS = false
	exerciseAtomics()
	if hasAtomics {
		cpu.ARM64.HasATOMICS = true
		exerciseAtomics()
	}
	cpu.ARM64.HasATOMICS = hasAtomics
}
