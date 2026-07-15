package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestARM64RuntimeExitTerminatesTheProcess(t *testing.T) {
	if !strings.Contains(runtimeARM64Assembly, "runtime_exit:\n\tmov x8, 94") {
		t.Fatal("runtime_exit must use Linux exit_group")
	}
	if !strings.Contains(runtimeARM64Assembly, "runtime_exitThread:\n\tstlr wzr, [x0]\n\tmov x0, 0\n\tmov x8, 93") {
		t.Fatal("runtime_exitThread must use Linux thread exit")
	}
}

func TestRuntimeSupportDoesNotShadowTranslatedStandardLibraryAssembly(t *testing.T) {
	for _, symbol := range []string{
		"runtime_memmove",
		"runtime_memclrNoHeapPointers",
		"runtime_publicationBarrier",
	} {
		if strings.Contains(runtimeARM64Assembly, ".global "+symbol) {
			t.Errorf("handwritten runtime support still defines %s", symbol)
		}
	}
}

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

func TestARM64RuntimeAtomicSupportExecutes(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("AArch64 runtime support")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("cc unavailable")
	}
	directory := t.TempDir()
	assembly := filepath.Join(directory, "runtime.S")
	harness := filepath.Join(directory, "atomic.c")
	executable := filepath.Join(directory, "atomic")
	if err := os.WriteFile(assembly, []byte(runtimeARM64Assembly), 0o644); err != nil {
		t.Fatal(err)
	}
	const source = `
#include <stdint.h>

uint32_t internal_runtime_atomic_Load(uint32_t *);
void internal_runtime_atomic_Store(uint32_t *, uint32_t);
uint32_t internal_runtime_atomic_Xadd(uint32_t *, int32_t);
uint32_t internal_runtime_atomic_Xchg(uint32_t *, uint32_t);
int internal_runtime_atomic_Cas(uint32_t *, uint32_t, uint32_t);

int main(void) {
	uint32_t value = 7;
	if (internal_runtime_atomic_Load(&value) != 7) return 1;
	internal_runtime_atomic_Store(&value, 11);
	if (value != 11) return 2;
	if (internal_runtime_atomic_Xadd(&value, 5) != 16 || value != 16) return 3;
	if (!internal_runtime_atomic_Cas(&value, 16, 23) || value != 23) return 4;
	if (internal_runtime_atomic_Cas(&value, 16, 29) || value != 23) return 5;
	if (internal_runtime_atomic_Xchg(&value, 31) != 23 || value != 31) return 6;
	return 0;
}
`
	if err := os.WriteFile(harness, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(cc, "-no-pie", "-o", executable, harness, assembly).CombinedOutput(); err != nil {
		t.Fatalf("compile runtime support: %v\n%s", err, output)
	}
	if output, err := exec.Command(executable).CombinedOutput(); err != nil {
		t.Fatalf("execute runtime support: %v\n%s", err, output)
	}
}

func TestARM64GoRuntimeGarbageCollectorExecutes(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("AArch64 Go runtime bootstrap")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc unavailable")
	}

	directory := t.TempDir()
	compiler := filepath.Join(directory, "goc")
	build := exec.Command("go", "build", "-o", compiler, ".")
	build.Env = append(os.Environ(), "GOCACHE="+filepath.Join(directory, "cache"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build compiler: %v\n%s", err, output)
	}

	program := filepath.Join("..", "..", "goc", "testdata", "gc_struct.go")
	executable := filepath.Join(directory, "gc-struct")
	compile := exec.Command(compiler, "-o", executable, program)
	if output, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compile GC program: %v\n%s", err, output)
	}
	if output, err := exec.Command(executable).CombinedOutput(); err != nil {
		t.Fatalf("execute GC program: %v\n%s", err, output)
	}
}

func TestARM64StandardLibraryIOAndFmtExecute(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("AArch64 Go runtime bootstrap")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc unavailable")
	}

	directory := t.TempDir()
	compiler := filepath.Join(directory, "goc")
	build := exec.Command("go", "build", "-o", compiler, ".")
	build.Env = append(os.Environ(), "GOCACHE="+filepath.Join(directory, "cache"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build compiler: %v\n%s", err, output)
	}

	tests := []struct {
		name   string
		source string
		output string
	}{
		{name: "io.WriteString", source: "io_write_string.go"},
		{name: "fmt.Sprintf", source: "fmt_sprintf.go"},
		{name: "fmt.Println", source: "fmt_println.go", output: "hello 42\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			program := filepath.Join("..", "..", "goc", "testdata", test.source)
			executable := filepath.Join(directory, test.source+".bin")
			compile := exec.Command(compiler, "-o", executable, program)
			if output, err := compile.CombinedOutput(); err != nil {
				t.Fatalf("compile %s: %v\n%s", test.source, err, output)
			}

			output, err := exec.Command(executable).CombinedOutput()
			if err != nil {
				t.Fatalf("execute %s: %v\n%s", test.source, err, output)
			}
			if string(output) != test.output {
				t.Fatalf("output from %s = %q, want %q", test.source, output, test.output)
			}
		})
	}
}

func TestARM64PlainMainBootsGoRuntimeAndRunsInit(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("AArch64 Go runtime bootstrap")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc unavailable")
	}

	directory := t.TempDir()
	compiler := filepath.Join(directory, "goc")
	build := exec.Command("go", "build", "-o", compiler, ".")
	build.Env = append(os.Environ(), "GOCACHE="+filepath.Join(directory, "cache"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build compiler: %v\n%s", err, output)
	}

	source := filepath.Join(directory, "main.go")
	program := `package main

var initialized int

func init() {
	initialized = 41
}

func main() {
	if initialized != 41 {
		panic("package init did not run")
	}
}
`
	if err := os.WriteFile(source, []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "plain-main")
	compile := exec.Command(compiler, "-o", executable, source)
	if output, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compile plain Go program: %v\n%s", err, output)
	}
	if output, err := exec.Command(executable).CombinedOutput(); err != nil {
		t.Fatalf("execute plain Go program: %v\n%s", err, output)
	}
}
