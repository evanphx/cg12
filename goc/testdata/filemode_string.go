package main

import "io/fs"

func main() {
	if fs.FileMode(0o644).String() != "-rw-r--r--" {
		panic("unexpected file mode")
	}
}
