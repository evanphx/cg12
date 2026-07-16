package main

type globalStructSliceEntry struct {
	name       string
	firstHash  uint32
	secondHash uint32
	value      *uintptr
}

var firstGlobalStructSliceValue uintptr
var secondGlobalStructSliceValue uintptr

var globalStructSliceEntries = []globalStructSliceEntry{
	{"alpha", 0x0b0cd725, 0xdfa941fd, &firstGlobalStructSliceValue},
	{"beta", 0x09800c0d, 0x540d4e24, &secondGlobalStructSliceValue},
}

func main() {
	if globalStructSliceEntries[0].name != "alpha" {
		panic("first global struct slice entry was not initialized")
	}
	if globalStructSliceEntries[1].name != "beta" {
		panic("second global struct slice entry was not initialized")
	}
	if globalStructSliceEntries[0].firstHash != 0x0b0cd725 {
		panic("first global struct slice hash was not initialized")
	}
	if globalStructSliceEntries[0].secondHash != 0xdfa941fd {
		panic("second global struct slice hash was not initialized")
	}
	if globalStructSliceEntries[1].firstHash != 0x09800c0d {
		panic("third global struct slice hash was not initialized")
	}
	if globalStructSliceEntries[1].secondHash != 0x540d4e24 {
		panic("fourth global struct slice hash was not initialized")
	}

	*globalStructSliceEntries[0].value = 17
	*globalStructSliceEntries[1].value = 25
	if firstGlobalStructSliceValue+secondGlobalStructSliceValue != 42 {
		panic("global struct slice pointer fields were not initialized")
	}
}
