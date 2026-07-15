package main

import (
	"fmt"
	"runtime"
)

type assemblyRecord struct {
	index   int
	payload [24]byte
}

func main() {
	// Repeated slice growth makes runtime.growslice preserve the old backing
	// array with runtime.memmove, which comes from runtime/memmove_arm64.s.
	records := make([]assemblyRecord, 0, 1)
	for index := 1; index <= 256; index++ {
		record := assemblyRecord{index: index}
		for offset := range record.payload {
			record.payload[offset] = byte((index+offset)%251 + 1)
		}
		records = append(records, record)
	}

	checksum := 0
	for _, record := range records {
		want := byte((record.index+7)%251 + 1)
		if record.payload[7] != want {
			panic("runtime.memmove corrupted a grown slice")
		}
		checksum += record.index
	}

	// Heap allocation and collection exercise runtime.memclrNoHeapPointers and
	// runtime.publicationBarrier from the translated standard-library sources.
	buffer := make([]byte, 4096)
	for index := range buffer {
		buffer[index] = byte(index%251 + 1)
	}
	clear(buffer[512:3584])
	runtime.GC()

	zeroBytes := 0
	for _, value := range buffer {
		if value == 0 {
			zeroBytes++
		}
	}

	fmt.Println("ABIInternal + Plan 9 assembly:", len(records), checksum, zeroBytes)
}
