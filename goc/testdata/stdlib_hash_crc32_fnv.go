package main

import (
	"hash/crc32"
	"hash/fnv"
)

func main() {
	data := []byte("cg12 hash coverage")
	checksum := crc32.ChecksumIEEE(data)
	if checksum != crc32.Update(0, crc32.IEEETable, data) {
		panic("crc32 checksum/update mismatch")
	}

	hash := fnv.New64a()
	if _, err := hash.Write([]byte("cg12")); err != nil {
		panic("fnv Write failed")
	}
	if _, err := hash.Write([]byte(" hash coverage")); err != nil {
		panic("fnv second Write failed")
	}
	if hash.Sum64() == 0 {
		panic("fnv returned zero hash")
	}
}
