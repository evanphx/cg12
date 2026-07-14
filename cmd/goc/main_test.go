package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDriverIRAndRun(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc unavailable")
	}
	d := t.TempDir()
	bin := filepath.Join(d, "goc")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(d, "cache"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	src := filepath.Join(d, "main.go")
	program := `package main

func main() {
	s := 0
	for i := 1; i <= 10; i++ {
		s += i
	}
	if s != 55 {
		for {
		}
	}
}
`
	if err := os.WriteFile(src, []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "-emit-ir", src).CombinedOutput(); err != nil {
		t.Fatalf("IR: %v\n%s", err, out)
	}
	if out, err := exec.Command(bin, "-run", "-o", filepath.Join(d, "prog"), src).CombinedOutput(); err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
}
