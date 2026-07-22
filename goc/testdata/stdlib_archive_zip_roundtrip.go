package main

import (
	"archive/zip"
	"bytes"
	"io"
	"io/fs"
	"time"
)

func main() {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	if err := writer.SetComment("cg12 zip status"); err != nil {
		panic(err)
	}

	modificationTime := time.Unix(1_700_000_000, 0).UTC()
	header := &zip.FileHeader{
		Name:     "docs/status.txt",
		Method:   zip.Deflate,
		Modified: modificationTime,
	}
	header.SetMode(0o640)
	file, err := writer.CreateHeader(header)
	if err != nil {
		panic(err)
	}
	if _, err := io.WriteString(file, "cg12 archive status\n"); err != nil {
		panic(err)
	}

	storedHeader := &zip.FileHeader{Name: "raw/value.bin", Method: zip.Store}
	storedHeader.SetMode(0o600)
	stored, err := writer.CreateHeader(storedHeader)
	if err != nil {
		panic(err)
	}
	if _, err := stored.Write([]byte{1, 3, 5, 7, 9}); err != nil {
		panic(err)
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}

	reader, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		panic(err)
	}
	if reader.Comment != "cg12 zip status" || len(reader.File) != 2 {
		panic("zip directory metadata mismatch")
	}
	if reader.File[0].Name != "docs/status.txt" || reader.File[0].Method != zip.Deflate {
		panic("zip deflated header mismatch")
	}
	if reader.File[0].Mode().Perm() != 0o640 {
		panic("zip file mode mismatch")
	}
	if !reader.File[0].Modified.Equal(modificationTime) {
		panic("zip modification time mismatch")
	}

	contents, err := fs.ReadFile(reader, "docs/status.txt")
	if err != nil {
		panic(err)
	}
	if string(contents) != "cg12 archive status\n" {
		panic("zip deflated contents mismatch")
	}
	raw, err := fs.ReadFile(reader, "raw/value.bin")
	if err != nil {
		panic(err)
	}
	if !bytes.Equal(raw, []byte{1, 3, 5, 7, 9}) {
		panic("zip stored contents mismatch")
	}
}
