// Command goc compiles a Go source file through the cg12 backend.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

//go:embed runtime_arm64.S
var runtimeARM64Assembly string

//go:embed bootstrap_arm64.S
var bootstrapARM64Assembly string

func main() {
	out := flag.String("o", "", "output file")
	obj := flag.Bool("c", false, "emit a relocatable object")
	asm := flag.Bool("S", false, "emit assembly")
	emitIR := flag.Bool("emit-ir", false, "print cg12 IR")
	optimize := flag.Bool("O", false, "optimize cg12 IR")
	run := flag.Bool("run", false, "link and run the program")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: goc [-O] [-o out] [-c|-S|-emit-ir|-run] file.go")
		os.Exit(2)
	}
	input := flag.Arg(0)
	src, err := os.ReadFile(input)
	check(err)
	m, err := goc.Compile(filepath.Base(input), src)
	check(err)
	if *optimize {
		opt.OptimizeModule(m)
	}
	switch {
	case *emitIR:
		fmt.Print(m)
	case *asm:
		s, err := compileAsm(m)
		check(err)
		write(path(*out, input, ".s"), []byte(s))
	case *obj:
		b, err := compileObject(m)
		check(err)
		write(path(*out, input, ".o"), b)
	default:
		exe := *out
		if exe == "" {
			exe = goc.OutputName(input)
		}
		link(m, exe)
		if *run {
			os.Exit(runProgram(exe))
		}
	}
}

func compileAsm(m *ir.Module) (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "", fmt.Errorf("assembly display is not available for the object-only amd64 backend")
	case "arm64":
		object, err := arm64.CompileToObject(m)
		if err != nil {
			return "", err
		}
		return arm64.Disassemble(object), nil
	}
	return "", fmt.Errorf("unsupported host architecture %s", runtime.GOARCH)
}
func compileObject(m *ir.Module) ([]byte, error) {
	switch runtime.GOARCH {
	case "amd64":
		return amd64.CompileObject(m)
	case "arm64":
		return arm64.CompileObject(m)
	}
	return nil, fmt.Errorf("unsupported host architecture %s", runtime.GOARCH)
}
func path(out, input, ext string) string {
	if out != "" {
		return out
	}
	return strings.TrimSuffix(filepath.Base(input), filepath.Ext(input)) + ext
}
func write(name string, b []byte) {
	check(os.WriteFile(name, b, 0o644))
}

func link(m *ir.Module, exe string) {
	b, err := compileObject(m)
	check(err)
	f, err := os.CreateTemp("", "cg12-goc-*.o")
	check(err)
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	check(err)
	check(f.Close())
	cc, err := exec.LookPath("cc")
	check(err)
	inputs := []string{f.Name()}
	if runtime.GOARCH == "arm64" {
		assembly := runtimeARM64Assembly
		if usesGoRuntime(m) {
			assembly += "\n" + bootstrapARM64Assembly
		}
		support, cleanup := compileRuntimeSupport(cc, assembly)
		defer cleanup()
		inputs = append(inputs, support)
	}
	args := append([]string{"-no-pie", "-o", exe}, inputs...)
	cmd := exec.Command(cc, args...)
	cmd.Stderr = os.Stderr
	check(cmd.Run())
}

func usesGoRuntime(module *ir.Module) bool {
	for _, function := range module.Funcs {
		if function.Name == "runtime.schedinit" {
			return true
		}
	}
	return false
}

func compileRuntimeSupport(cc, assembly string) (string, func()) {
	source, err := os.CreateTemp("", "cg12-goc-runtime-*.S")
	check(err)
	object := strings.TrimSuffix(source.Name(), ".S") + ".o"
	cleanup := func() {
		os.Remove(source.Name())
		os.Remove(object)
	}
	_, err = source.WriteString(assembly)
	if err == nil {
		err = source.Close()
	}
	if err != nil {
		cleanup()
		check(err)
	}
	cmd := exec.Command(cc, "-c", "-o", object, source.Name())
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		check(err)
	}
	return object, cleanup
}

func runProgram(name string) int {
	abs, err := filepath.Abs(name)
	check(err)
	cmd := exec.Command(abs)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	if e, ok := err.(*exec.ExitError); ok {
		return e.ExitCode()
	}
	check(err)
	return 0
}

func check(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "goc: %v\n", err)
		os.Exit(1)
	}
}
