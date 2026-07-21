package main

import (
	"os/exec"
	"strings"
)

func main() {
	output, err := exec.Command("/bin/echo", "cg12 exec status").Output()
	if err != nil {
		panic("exec echo failed")
	}
	if strings.TrimSpace(string(output)) != "cg12 exec status" {
		panic("exec echo output mismatch")
	}
}
