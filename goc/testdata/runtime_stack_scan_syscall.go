// A goroutine sitting in a system call is scanned through a different path than
// a running or a parked one.
//
// scanstack takes the goroutine's saved syscall stack pointer rather than its
// current one when gp.syscallsp is non-zero, and suspendG on a _Gsyscall
// goroutine does not have to preempt anything -- it takes the goroutine's stack
// while the thread is inside the kernel. If the saved syscall frame boundary or
// the stack map at the syscall's return PC is wrong, the frames underneath it
// are scanned against the wrong geometry and the roots below are lost.
//
// Two shapes, because Go reaches the kernel two ways and only one of them leaves
// the goroutine in _Gsyscall:
//
//   - syscall.Read on a raw blocking pipe file descriptor. The goroutine calls
//     entersyscall, blocks in read(2), and sysmon retakes its P; it is scanned
//     in _Gsyscall.
//   - os.File.Read on an os.Pipe. The descriptor is registered with the poller,
//     so the goroutine parks in netpoll and is scanned in _Gwaiting with a
//     runtime_pollWait frame on top.
//
// Each reader holds the only reference to its objects in its own frame across
// the call, and the main goroutine collects repeatedly before unblocking it.
package main

import (
	"os"
	"runtime"
	"sync"
	"syscall"
)

type record struct {
	value int
	label string
	next  *record
}

//go:noinline
func makeRecord(seed int) *record {
	return &record{
		value: seed,
		label: "record-" + string(rune('a'+seed%26)),
		next:  &record{value: seed * 5, label: "chained"},
	}
}

//go:noinline
func verify(where string, object *record, seed int) {
	want := "record-" + string(rune('a'+seed%26))
	if object == nil || object.value != seed || object.label != want {
		println("in", where, "lost its record")
		panic("a goroutine blocked in a system call lost a stack root")
	}
	if object.next == nil || object.next.value != seed*5 || object.next.label != "chained" {
		println("in", where, "lost its chained record")
		panic("a goroutine blocked in a system call lost an indirect stack root")
	}
}

//go:noinline
func churn() {
	var sink []*record
	for index := 0; index < 2048; index++ {
		sink = append(sink, &record{value: index, label: "churn"})
	}
	if len(sink) != 2048 {
		panic("churn lost its slice")
	}
}

//go:noinline
func collectWhileBlocked() {
	for cycle := 0; cycle < 3; cycle++ {
		runtime.GC()
		churn()
		runtime.GC()
	}
}

func main() {
	var rawPipe [2]int
	if err := syscall.Pipe(rawPipe[:]); err != nil {
		println("pipe:", err.Error())
		panic("could not create a raw pipe")
	}
	polledReader, polledWriter, err := os.Pipe()
	if err != nil {
		println("os.Pipe:", err.Error())
		panic("could not create a polled pipe")
	}

	var started sync.WaitGroup
	var finished sync.WaitGroup

	started.Add(2)
	finished.Add(2)

	// _Gsyscall: a raw blocking read.
	go func() {
		object := makeRecord(11)
		buffer := make([]byte, 4)
		started.Done()
		count, err := syscall.Read(rawPipe[0], buffer)
		if err != nil || count != 4 || string(buffer) != "raw!" {
			println("raw read count", count)
			panic("the raw syscall read did not deliver its bytes")
		}
		verify("raw syscall read", object, 11)
		finished.Done()
	}()

	// _Gwaiting in netpoll: a polled read.
	go func() {
		object := makeRecord(12)
		buffer := make([]byte, 4)
		started.Done()
		count, err := polledReader.Read(buffer)
		if err != nil || count != 4 || string(buffer) != "poll" {
			println("polled read count", count)
			panic("the polled read did not deliver its bytes")
		}
		verify("polled read", object, 12)
		finished.Done()
	}()

	started.Wait()
	collectWhileBlocked()

	if _, err := syscall.Write(rawPipe[1], []byte("raw!")); err != nil {
		println("raw write:", err.Error())
		panic("could not write to the raw pipe")
	}
	runtime.GC()
	if _, err := polledWriter.Write([]byte("poll")); err != nil {
		println("polled write:", err.Error())
		panic("could not write to the polled pipe")
	}

	finished.Wait()

	if err := syscall.Close(rawPipe[0]); err != nil {
		panic("could not close the raw pipe read end")
	}
	if err := syscall.Close(rawPipe[1]); err != nil {
		panic("could not close the raw pipe write end")
	}
	if err := polledReader.Close(); err != nil {
		panic("could not close the polled pipe read end")
	}
	if err := polledWriter.Close(); err != nil {
		panic("could not close the polled pipe write end")
	}
	println("syscall stack roots ok")
}
