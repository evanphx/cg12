package gometa

import "sort"

// A pc-value table is a run of (value delta, pc delta) pairs terminated by a
// zero value delta. The value delta is a zig-zag varint, the pc delta an
// unsigned varint counted in PC units -- which is why every encoder below
// divides a byte-valued PC by Arch.PCQuantum. The last pair's pc delta is
// ^uint32(0), the sentinel that makes the entry cover the rest of the function
// so decoding never runs into the next one.
//
// The tables all share one encoder because they share one wire format; only the
// value sequence differs.
type pcValueTable struct {
	data []byte
}

func (table *pcValueTable) uvarint(value uint32) {
	for value >= 0x80 {
		table.data = append(table.data, byte(value)|0x80)
		value >>= 7
	}
	table.data = append(table.data, byte(value))
}

func (table *pcValueTable) varint(value int) {
	encoded := uint32(value << 1)
	if value < 0 {
		encoded = ^encoded
	}
	table.uvarint(encoded)
}

// end writes the terminating zero value delta.
func (table *pcValueTable) end() {
	table.uvarint(0)
}

// pcUnits converts a byte-valued PC delta to the PC units the tables encode.
func (arch Arch) pcUnits(bytes int) uint32 {
	return uint32(bytes / arch.PCQuantum)
}

// PCValueTable encodes a table that is `value` for the whole function, except
// that a function with a stack-growth prologue reads 0 until frameStart. The
// initial delta is taken from -1, the value the runtime assumes where a table
// has no entry.
func (arch Arch) PCValueTable(frameStart, value int) []byte {
	var table pcValueTable
	current := -1
	if frameStart > 0 {
		table.varint(0)
		table.uvarint(arch.pcUnits(frameStart))
	}
	table.varint(value - current)
	table.uvarint(^uint32(0))
	table.end()
	return table.data
}

// PCSP encodes the frame-size table the runtime unwinds with: 0 while the
// prologue has not yet built the frame, then frameSize for the rest of the
// function.
func (arch Arch) PCSP(frameStart, frameSize int) []byte {
	type point struct {
		pc    int
		value int
	}
	points := make([]point, 0, 2)
	if frameStart > 0 {
		points = append(points, point{pc: 0, value: 0})
	}
	points = append(points, point{pc: frameStart, value: frameSize})

	var table pcValueTable
	value := -1
	for index, point := range points {
		table.varint(point.value - value)
		value = point.value
		if index+1 < len(points) {
			table.uvarint(arch.pcUnits(points[index+1].pc - point.pc))
		} else {
			table.uvarint(^uint32(0))
		}
	}
	table.end()
	return table.data
}

// UnsafePointPCData marks the complete generated function as unsafe for
// asynchronous preemption. cg12 keeps managed references in registers between
// calls, while its Go stack maps describe the spill state at call safepoints.
// Cooperative preemption at calls remains available.
func UnsafePointPCData() []byte {
	return []byte{1, 0xff, 0xff, 0xff, 0xff, 0x0f, 0}
}

// StackMapPCData encodes PCDATA_StackMapIndex: which of the function's pointer
// maps is live at each PC. It selects the entry map while the function is in its
// stack-growth prologue, then switches to the body map once the frame exists,
// then follows the safepoints. Safepoints before frameStart are dropped, points
// sharing a PC collapse to the last one, and runs of equal indexes collapse to
// their first, since a pc-value table only records changes.
func (arch Arch) StackMapPCData(frameStart int, indexPoints []StackMapIndexPoint) []byte {
	points := make([]StackMapIndexPoint, 0, len(indexPoints)+2)
	if frameStart > 0 {
		points = append(points, StackMapIndexPoint{PC: 0, Index: 0})
	}
	points = append(points, StackMapIndexPoint{PC: frameStart, Index: 1})
	for _, point := range indexPoints {
		if point.PC >= frameStart {
			points = append(points, point)
		}
	}
	sort.SliceStable(points, func(left, right int) bool {
		return points[left].PC < points[right].PC
	})

	unique := points[:0]
	for _, point := range points {
		if len(unique) > 0 && unique[len(unique)-1].PC == point.PC {
			unique[len(unique)-1] = point
			if len(unique) > 1 && unique[len(unique)-2].Index == point.Index {
				unique = unique[:len(unique)-1]
			}
			continue
		}
		if len(unique) > 0 && unique[len(unique)-1].Index == point.Index {
			continue
		}
		unique = append(unique, point)
	}

	var table pcValueTable
	value := -1
	for index, point := range unique {
		table.varint(point.Index - value)
		value = point.Index
		if index+1 < len(unique) {
			table.uvarint(arch.pcUnits(unique[index+1].PC - point.PC))
		} else {
			table.uvarint(^uint32(0))
		}
	}
	table.end()
	return table.data
}
