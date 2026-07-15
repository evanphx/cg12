package main

import (
	"internal/runtime/sys"
	"internal/runtime/syscall/linux"
	"syscall"
)

const arm64Getpid = 172

func checkSyscall6() {
	processID, _, errno := linux.Syscall6(arm64Getpid, 0, 0, 0, 0, 0, 0)
	if errno != 0 {
		panic("getpid returned an error")
	}
	if processID == 0 || processID == ^uintptr(0) {
		panic("getpid returned an invalid process ID")
	}
	if uintptr(syscall.Getpid()) != processID {
		panic("syscall.Getpid returned a different process ID")
	}

	result, _, errno := linux.Syscall6(^uintptr(0), 0, 0, 0, 0, 0, 0)
	if result != ^uintptr(0) {
		panic("invalid syscall returned an unexpected result")
	}
	if errno == 0 {
		panic("invalid syscall did not return an error")
	}
}

func checkDIT() {
	if !sys.DITSupported {
		return
	}

	wasEnabled := sys.DITEnabled()
	previous := sys.EnableDIT()
	if previous != wasEnabled {
		panic("EnableDIT returned the wrong previous state")
	}
	if !sys.DITEnabled() {
		panic("EnableDIT did not enable DIT")
	}

	if !wasEnabled {
		sys.DisableDIT()
		if sys.DITEnabled() {
			panic("DisableDIT did not disable DIT")
		}
	}
}

func main() {
	checkSyscall6()
	checkDIT()
}
