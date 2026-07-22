package main

import (
	"archive/tar"
	"bytes"
	"io"
	"time"
)

type tarEntry struct {
	name     string
	contents string
	mode     int64
}

func main() {
	entries := []tarEntry{
		{name: "README.txt", contents: "cg12 archive status\n", mode: 0o644},
		{
			name:     "deep/path/that/requires/a/pax/header/because/its/name/is/longer/than/the/classic/tar/name/field/status.txt",
			contents: "pax payload",
			mode:     0o600,
		},
	}

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	modificationTime := time.Unix(1_700_000_000, 0).UTC()
	for _, entry := range entries {
		header := &tar.Header{
			Name:    entry.name,
			Mode:    entry.mode,
			Size:    int64(len(entry.contents)),
			ModTime: modificationTime,
		}
		if err := writer.WriteHeader(header); err != nil {
			panic(err)
		}
		if _, err := io.WriteString(writer, entry.contents); err != nil {
			panic(err)
		}
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}

	reader := tar.NewReader(bytes.NewReader(archive.Bytes()))
	for index, expected := range entries {
		header, err := reader.Next()
		if err != nil {
			panic(err)
		}
		if header.Name != expected.name || header.Mode != expected.mode {
			panic("tar header mismatch")
		}
		if !header.ModTime.Equal(modificationTime) {
			panic("tar modification time mismatch")
		}
		contents, err := io.ReadAll(reader)
		if err != nil {
			panic(err)
		}
		if string(contents) != expected.contents {
			panic("tar contents mismatch")
		}
		if index == 0 && header.Format != tar.FormatUSTAR {
			panic("short tar entry did not use USTAR")
		}
	}
	if _, err := reader.Next(); err != io.EOF {
		panic("tar archive did not end at EOF")
	}
}
