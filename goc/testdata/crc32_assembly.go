package main

import "hash/crc32"

func main() {
	input := []byte("123456789")
	if checksum := crc32.ChecksumIEEE(input); checksum != 0xcbf43926 {
		panic("IEEE CRC-32 assembly returned the wrong checksum")
	}
	if checksum := crc32.Checksum(input, crc32.MakeTable(crc32.Castagnoli)); checksum != 0xe3069283 {
		panic("Castagnoli CRC-32 assembly returned the wrong checksum")
	}
}
