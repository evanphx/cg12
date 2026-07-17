package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanphx/cg12/internal/testenv"
)

// TestDriver builds the cc driver and exercises its modes end-to-end on a small
// program: -run executes it, -c produces an object, -dis shows the code.
func TestDriver(t *testing.T) {
	testenv.Tool(t, "gcc")
	dir := t.TempDir()
	bin := filepath.Join(dir, "cc")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build driver: %v\n%s", err, out)
	}

	srcPath := filepath.Join(dir, "prog.c")
	src := `#include <stdio.h>
int main(void){ int s=0; for(int i=1;i<=100;i++) s+=i; printf("%d\n", s); return 0; }`
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	// -run: compile, link, execute; check stdout.
	out, err := exec.Command(bin, "-run", srcPath).Output()
	if err != nil {
		t.Fatalf("-run: %v", err)
	}
	if string(out) != "5050\n" {
		t.Fatalf("-run output = %q, want %q", out, "5050\n")
	}

	// -c: produces a relocatable object.
	obj := filepath.Join(dir, "prog.o")
	if out, err := exec.Command(bin, "-c", "-o", obj, srcPath).CombinedOutput(); err != nil {
		t.Fatalf("-c: %v\n%s", err, out)
	}
	if fi, err := os.Stat(obj); err != nil || fi.Size() == 0 {
		t.Fatalf("-c did not produce an object: %v", err)
	}

	// -dis: shows the generated code, read back out of the object -- labelled with
	// the function it belongs to and naming the symbols it calls.
	dis, err := exec.Command(bin, "-dis", srcPath).Output()
	if err != nil {
		t.Fatalf("-dis: %v", err)
	}
	for _, want := range []string{"main:", "\tret\n", "\tbl printf\n"} {
		if !strings.Contains(string(dis), want) {
			t.Fatalf("-dis output missing %q:\n%s", want, dis)
		}
	}
}
