package main

import "sync/atomic"

func main() {
	var word uint32
	atomic.StoreUint32(&word, 10)
	if atomic.LoadUint32(&word) != 10 {
		panic("atomic 32-bit load or store failed")
	}
	if atomic.AddUint32(&word, 5) != 15 {
		panic("atomic 32-bit add failed")
	}
	if !atomic.CompareAndSwapUint32(&word, 15, 20) {
		panic("atomic 32-bit compare-and-swap failed")
	}
	if atomic.SwapUint32(&word, 7) != 20 {
		panic("atomic 32-bit exchange failed")
	}
	if atomic.OrUint32(&word, 8) != 7 || word != 15 {
		panic("atomic 32-bit OR failed")
	}
	if atomic.AndUint32(&word, 6) != 15 || word != 6 {
		panic("atomic 32-bit AND failed")
	}

	var wide uint64
	atomic.StoreUint64(&wide, 100)
	if atomic.LoadUint64(&wide) != 100 {
		panic("atomic 64-bit load or store failed")
	}
	if atomic.AddUint64(&wide, 25) != 125 {
		panic("atomic 64-bit add failed")
	}
	if !atomic.CompareAndSwapUint64(&wide, 125, 200) {
		panic("atomic 64-bit compare-and-swap failed")
	}
	if atomic.SwapUint64(&wide, 9) != 200 {
		panic("atomic 64-bit exchange failed")
	}
	if atomic.OrUint64(&wide, 6) != 9 || wide != 15 {
		panic("atomic 64-bit OR failed")
	}
	if atomic.AndUint64(&wide, 10) != 15 || wide != 10 {
		panic("atomic 64-bit AND failed")
	}
}
