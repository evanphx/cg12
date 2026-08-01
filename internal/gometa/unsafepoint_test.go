package gometa

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// UnsafePointPCData is the reason asynchronous preemption never happens in a
// cg12-compiled program, and therefore the reason runtime.scanConservative is
// unreachable: isAsyncSafePoint reads PCDATA_UnsafePoint at the interrupted PC
// and refuses to inject runtime.asyncPreempt when it reads
// abi.UnsafePointUnsafe.
//
// That is deliberate -- cg12 keeps managed references in registers between calls
// while its stack maps describe the spill state at call safepoints -- and
// RUNTIME_PLAN.md section 6.1 classifies the conservative scan as unreachable on
// the strength of it. A classification is only as good as the thing it points
// at, so this decodes the table the same way runtime.pcvalue does and checks
// that it really does say "unsafe" from the entry PC to the end of the address
// space. A table that quietly decoded to UnsafePointSafe would leave the
// classification claiming something that is not true.
func TestUnsafePointPCDataMarksTheWholeFunctionUnsafe(t *testing.T) {
	// internal/abi: UnsafePointSafe is -1 and UnsafePointUnsafe is -2.
	const unsafePointUnsafe = -2

	table := UnsafePointPCData()
	require.NotEmpty(t, table)

	value, pcSpan, entries := decodePCValueTable(t, table)
	require.Equal(t, 1, entries, "the table must have exactly one entry: one value for the whole function")
	require.EqualValues(t, unsafePointUnsafe, value,
		"a generated function must read as abi.UnsafePointUnsafe, or isAsyncSafePoint will inject asyncPreempt")
	require.EqualValues(t, ^uint32(0), pcSpan,
		"the entry must cover the whole function, not a prefix of it")
}

// decodePCValueTable walks a pc-value table exactly as runtime.pcvalue's step
// does: a zigzag-encoded value delta followed by a uvarint PC delta, repeated,
// terminated by a zero byte. The initial value is -1.
func decodePCValueTable(t *testing.T, table []byte) (value int32, pcSpan uint32, entries int) {
	t.Helper()

	value = -1
	offset := 0
	for offset < len(table) && table[offset] != 0 {
		encoded, read := readUvarint(t, table[offset:])
		offset += read
		var delta int32
		if encoded&1 != 0 {
			delta = int32(^(encoded >> 1))
		} else {
			delta = int32(encoded >> 1)
		}
		value += delta

		pcDelta, read := readUvarint(t, table[offset:])
		offset += read
		pcSpan += pcDelta
		entries++
	}
	require.Less(t, offset, len(table), "the table is not terminated")
	require.EqualValues(t, 0, table[offset], "the table must end with a zero byte")
	require.Equal(t, len(table)-1, offset, "the table has trailing bytes after its terminator")
	return value, pcSpan, entries
}

func readUvarint(t *testing.T, data []byte) (uint32, int) {
	t.Helper()

	var value uint32
	var shift uint
	for index, b := range data {
		value |= uint32(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, index + 1
		}
		shift += 7
		require.Less(t, shift, uint(35), "uvarint is too long")
	}
	t.Fatal("uvarint is unterminated")
	return 0, 0
}
