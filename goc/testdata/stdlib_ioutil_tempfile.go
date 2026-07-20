package main

import (
	"io/ioutil"
	"os"
)

func main() {
	directory, err := ioutil.TempDir("", "cg12-ioutil-")
	if err != nil {
		panic("ioutil TempDir failed")
	}
	defer os.RemoveAll(directory)

	file, err := ioutil.TempFile(directory, "status-")
	if err != nil {
		panic("ioutil TempFile failed")
	}
	name := file.Name()
	if _, err := file.Write([]byte("ioutil status")); err != nil {
		panic("TempFile Write failed")
	}
	if err := file.Close(); err != nil {
		panic("TempFile Close failed")
	}

	data, err := ioutil.ReadFile(name)
	if err != nil {
		panic("ioutil ReadFile failed")
	}
	if string(data) != "ioutil status" {
		panic("ioutil ReadFile mismatch")
	}
}
