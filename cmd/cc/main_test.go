package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestDriver builds the cc driver and exercises its modes end-to-end on a small
// program: -run executes it, -c produces an object, -S produces assembly.
func TestDriver(t *testing.T) {
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("gcc not available")
	}
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

	// -S: produces assembly text that assembles, links, and runs correctly
	// (exercising string-data escaping and symbol naming in the text emitter).
	asm := filepath.Join(dir, "prog.s")
	if out, err := exec.Command(bin, "-S", "-o", asm, srcPath).CombinedOutput(); err != nil {
		t.Fatalf("-S: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(asm); err != nil || len(b) == 0 {
		t.Fatalf("-S did not produce assembly: %v", err)
	}
	asmExe := filepath.Join(dir, "prog_s")
	if out, err := exec.Command("gcc", asm, "-o", asmExe).CombinedOutput(); err != nil {
		t.Fatalf("assembling -S output: %v\n%s", err, out)
	}
	if out, err := exec.Command(asmExe).Output(); err != nil || string(out) != "5050\n" {
		t.Fatalf("-S program output = %q (err %v), want %q", out, err, "5050\n")
	}
}
