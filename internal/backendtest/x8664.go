package backendtest

// X8664StartStub is a freestanding entry point for an x86-64 image: call the
// exported entry `runtest`, then exit with what it returned.
//
// No libc, so the image needs nothing of its environment but the SYS_exit
// syscall -- true of the host's kernel and of an emulator's alike, which is what
// lets one image serve both execution paths. It also keeps the thing under test
// alone in the picture: nothing but the backend's own output stands between
// _start and the value the test compares.
const X8664StartStub = `
	.text
	.globl _start
_start:
	call runtest
	movl %eax, %edi
	movl $60, %eax
	syscall
`

// X8664Freestanding builds x86-64 images the way the amd64 backend tests want
// them: the backend's object plus [X8664StartStub], linked static and without
// libc by lld, assembled by clang aimed at x86-64 so the same recipe works on a
// host of any architecture.
//
// A module linked this way must export a function named "runtest" returning a
// word, whose return value becomes the image's exit status.
func X8664Freestanding() Harness {
	return Harness{
		Machine:     X8664,
		Compiler:    []string{"clang"},
		CompileArgs: []string{"--target=x86_64-linux-gnu"},
		Linker:      []string{"ld.lld"},
		LinkArgs:    []string{"-static", "-nostdlib"},
		StartStub:   X8664StartStub,
	}
}
