// Command goc compiles a Go source file through the cg12 backend.
package main

import (
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

func main() {
	if len(os.Args) > 1 && os.Args[1] == "test" {
		os.Exit(testCommand(os.Args[2:]))
	}

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
	buildExecutable := !*obj && !*asm && !*emitIR
	var m *ir.Module
	if buildExecutable && runtime.GOARCH == "arm64" {
		m, err = goc.CompileExecutable(filepath.Base(input), src)
	} else {
		m, err = goc.Compile(filepath.Base(input), src)
	}
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

func testCommand(arguments []string) int {
	flags := flag.NewFlagSet("goc test", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	runPattern := flags.String("run", "", "run only tests matching the regular expression")
	verbose := flags.Bool("v", false, "print each test as it runs")
	optimize := flags.Bool("O", false, "optimize cg12 IR")
	output := flags.String("o", "", "write the test executable to this file")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: goc test [-O] [-v] [-run regexp] [-o testbinary] package")
		return 2
	}
	if runtime.GOARCH != "arm64" {
		fmt.Fprintf(os.Stderr, "goc: test executables are not supported on %s\n", runtime.GOARCH)
		return 1
	}

	packagePath := flags.Arg(0)
	module, _, err := goc.CompileTestExecutableMatching(packagePath, *runPattern)
	if err != nil {
		fmt.Fprintf(os.Stderr, "goc: %v\n", err)
		return 1
	}
	if *optimize {
		opt.OptimizeModule(module)
	}

	executable := *output
	var temporaryDirectory string
	if executable == "" {
		temporaryDirectory, err = os.MkdirTemp("", "goc-test-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "goc: %v\n", err)
			return 1
		}
		defer os.RemoveAll(temporaryDirectory)
		executable = filepath.Join(temporaryDirectory, "testbinary")
	}
	link(module, executable)

	programArguments := make([]string, 0, 2)
	if *verbose {
		programArguments = append(programArguments, "-test.v=true")
	}
	if *runPattern != "" {
		programArguments = append(programArguments, "-test.run="+*runPattern)
	}
	return runProgram(executable, programArguments...)
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
	var translatedAssembly string
	f, err := os.CreateTemp("", "cg12-goc-*.o")
	check(err)
	defer os.Remove(f.Name())

	if runtime.GOARCH == "arm64" {
		translatedAssembly, err = arm64.WriteObjectAndAssembly(f, m)
	} else {
		var objectBytes []byte
		objectBytes, err = compileObject(m)
		if err == nil {
			_, err = f.Write(objectBytes)
		}
	}
	check(err)
	check(f.Close())
	cc, err := exec.LookPath("cc")
	check(err)
	inputs := []string{f.Name()}
	if runtime.GOARCH == "arm64" {
		support, cleanup := compileRuntimeSupport(cc, translatedAssembly)
		defer cleanup()
		inputs = append(inputs, support)
	}
	args := append([]string{"-no-pie", "-o", exe}, inputs...)
	cmd := exec.Command(cc, args...)
	cmd.Stderr = os.Stderr
	check(cmd.Run())
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

func runProgram(name string, arguments ...string) int {
	abs, err := filepath.Abs(name)
	check(err)
	cmd := exec.Command(abs, arguments...)
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
