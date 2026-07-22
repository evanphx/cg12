package main

import (
	"io/fs"
	"strings"
	"testing/fstest"
	"time"
)

func main() {
	modificationTime := time.Unix(1_700_000_000, 0)
	fileSystem := fstest.MapFS{
		"config/settings.txt": {
			Data:    []byte("target=arm64\n"),
			Mode:    0o644,
			ModTime: modificationTime,
		},
		"assets/logo.txt": {
			Data:    []byte("cg12\n"),
			Mode:    0o644,
			ModTime: modificationTime,
		},
	}
	if err := fstest.TestFS(fileSystem, "config/settings.txt", "assets/logo.txt"); err != nil {
		panic(err)
	}

	contents, err := fs.ReadFile(fileSystem, "config/settings.txt")
	if err != nil {
		panic(err)
	}
	if string(contents) != "target=arm64\n" {
		panic("MapFS file contents mismatch")
	}

	matches, err := fs.Glob(fileSystem, "config/*.txt")
	if err != nil {
		panic(err)
	}
	if len(matches) != 1 || matches[0] != "config/settings.txt" {
		panic("MapFS glob mismatch")
	}

	config, err := fs.Sub(fileSystem, "config")
	if err != nil {
		panic(err)
	}
	contents, err = fs.ReadFile(config, "settings.txt")
	if err != nil || string(contents) != "target=arm64\n" {
		panic("MapFS sub-filesystem mismatch")
	}

	var visited []string
	err = fs.WalkDir(fileSystem, ".", func(path string, entry fs.DirEntry, walkError error) error {
		if walkError != nil {
			return walkError
		}
		visited = append(visited, path)
		return nil
	})
	if err != nil {
		panic(err)
	}
	if strings.Join(visited, ",") != ".,assets,assets/logo.txt,config,config/settings.txt" {
		panic("MapFS walk order mismatch")
	}
}
