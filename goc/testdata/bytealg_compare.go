package main

import (
	"internal/bytealg"
	"unsafe"
)

//go:linkname runtimeMemequal runtime.memequal
func runtimeMemequal(left, right unsafe.Pointer, size uintptr) bool

func checkBytes(left, right string, want int) {
	got := bytealg.Compare([]byte(left), []byte(right))
	if got != want {
		panic("bytealg.Compare returned the wrong result")
	}
}

func checkStrings(left, right string, want int) {
	got := bytealg.CompareString(left, right)
	if got != want {
		panic("bytealg.CompareString returned the wrong result")
	}
}

func checkIndex(haystack, needle string, want int) {
	gotBytes := bytealg.Index([]byte(haystack), []byte(needle))
	if gotBytes != want {
		panic("bytealg.Index returned the wrong result")
	}
	gotString := bytealg.IndexString(haystack, needle)
	if gotString != want {
		panic("bytealg.IndexString returned the wrong result")
	}
}

func checkEqual(left, right string, want bool) {
	leftBytes := []byte(left)
	rightBytes := []byte(right)
	var leftPointer unsafe.Pointer
	var rightPointer unsafe.Pointer
	if len(leftBytes) > 0 {
		leftPointer = unsafe.Pointer(&leftBytes[0])
	}
	if len(rightBytes) > 0 {
		rightPointer = unsafe.Pointer(&rightBytes[0])
	}
	got := runtimeMemequal(leftPointer, rightPointer, uintptr(len(leftBytes)))
	if got != want {
		panic("runtime.memequal returned the wrong result")
	}
}

func checkIndexByte(value string, target byte, want int) {
	gotBytes := bytealg.IndexByte([]byte(value), target)
	if gotBytes != want {
		panic("bytealg.IndexByte returned the wrong result")
	}
	gotString := bytealg.IndexByteString(value, target)
	if gotString != want {
		panic("bytealg.IndexByteString returned the wrong result")
	}
}

func checkCount(value string, target byte, want int) {
	gotBytes := bytealg.Count([]byte(value), target)
	if gotBytes != want {
		panic("bytealg.Count returned the wrong result")
	}
	gotString := bytealg.CountString(value, target)
	if gotString != want {
		panic("bytealg.CountString returned the wrong result")
	}
}

func main() {
	checkBytes("", "", 0)
	checkBytes("a", "b", -1)
	checkBytes("b", "a", 1)
	checkBytes("prefix", "prefix-longer", -1)
	checkBytes("prefix-longer", "prefix", 1)
	checkBytes("0123456789abcdef-same-A", "0123456789abcdef-same-B", -1)
	checkBytes("0123456789abcdef-same-B", "0123456789abcdef-same-A", 1)
	checkBytes("0123456789abcdef-same", "0123456789abcdef-same", 0)

	checkStrings("short", "short", 0)
	checkStrings("0123456789abcdef-lower", "0123456789abcdef-upper", -1)
	checkStrings("0123456789abcdef-upper", "0123456789abcdef-lower", 1)

	checkIndex("xxabyy", "ab", 2)
	checkIndex("xxabcyy", "abc", 2)
	checkIndex("xxabcdefghyy", "abcdefgh", 2)
	checkIndex("xxabcdefghiyy", "abcdefghi", 2)
	checkIndex("xxabcdefghijklmnop", "abcdefghijklmnop", 2)
	checkIndex("xxabcdefghijklmnopq", "abcdefghijklmnopq", 2)
	checkIndex("xxabcdefghijklmnopqrstuvwx", "abcdefghijklmnopqrstuvwx", 2)
	checkIndex("xxabcdefghijklmnopqrstuvwxy", "abcdefghijklmnopqrstuvwxy", 2)
	checkIndex("xx0123456789abcdefghijklmnopqrstuvyy", "0123456789abcdefghijklmnopqrstuv", 2)
	checkIndex("no matching substring here", "absent", -1)

	checkEqual("", "", true)
	checkEqual("a", "a", true)
	checkEqual("a", "b", false)
	checkEqual("ab", "ab", true)
	checkEqual("abcd", "abce", false)
	checkEqual("12345678", "12345678", true)
	checkEqual("123456789abcdef", "123456789abcdeg", false)
	checkEqual("0123456789abcdef", "0123456789abcdef", true)
	checkEqual("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde", true)
	checkEqual("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeg", false)
	checkEqual("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdefx", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdefx", true)

	checkIndexByte("", 'x', -1)
	checkIndexByte("short", 'o', 2)
	checkIndexByte("0123456789abcdef0123456789abcdef-X", 'X', 33)
	checkIndexByte("0123456789abcdef0123456789abcdef", 'X', -1)

	checkCount("", 'x', 0)
	checkCount("mississippi", 's', 4)
	checkCount("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", 'x', 64)
	checkCount("0123456789abcdef0123456789abcdef", 'X', 0)
}
