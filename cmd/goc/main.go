// Command goc compiles a Go source file through the cg12 backend.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	if len(os.Args) > 1 && os.Args[1] == "runtime-cover-diff" {
		os.Exit(runtimeCoverageDiffCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "runtime-cover-baseline" {
		os.Exit(runtimeCoverageBaselineCommand(os.Args[2:], os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "build-runtime" {
		os.Exit(buildRuntimeCommand(os.Args[2:], os.Stderr))
	}

	out := flag.String("o", "", "output file")
	obj := flag.Bool("c", false, "emit a relocatable object")
	asm := flag.Bool("S", false, "emit assembly")
	emitIR := flag.Bool("emit-ir", false, "print cg12 IR")
	optimize := flag.Bool("O", false, "optimize cg12 IR")
	run := flag.Bool("run", false, "link and run the program")
	runtimeCoverMeta := flag.String("runtime-covermeta", "", "instrument runtime and write coverage metadata")
	prebuiltRuntime := flag.String("runtime", "", "link against the prebuilt runtimes written by `goc build-runtime`, comma-separated; the richest usable one is chosen")
	targetName := flag.String("target", defaultTargetName(), "arm64 | amd64")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: goc [-O] [-target arch] [-o out] [-runtime runtime.gocrt] [-c|-S|-emit-ir|-run] file.go")
		os.Exit(2)
	}
	// ParseTarget's message already names the command, so it is printed as-is
	// rather than through check, which would prefix "goc: " a second time.
	target, err := goc.ParseTarget(*targetName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	input := flag.Arg(0)
	src, err := os.ReadFile(input)
	check(err)
	buildExecutable := !*obj && !*asm
	if *prebuiltRuntime != "" {
		if !buildExecutable || *emitIR || *runtimeCoverMeta != "" {
			check(fmt.Errorf("-runtime builds an executable, so it cannot be combined with -c, -S, -emit-ir or -runtime-covermeta"))
		}
		exe := *out
		if exe == "" {
			exe = goc.OutputName(input)
		}
		check(linkAgainstPrebuiltRuntime(target, strings.Split(*prebuiltRuntime, ","), input, src, exe, *optimize))
		if *run {
			os.Exit(runProgram(exe))
		}
		return
	}
	var m *ir.Module
	var runtimeCoverage *goc.RuntimeCoverage
	if *runtimeCoverMeta != "" {
		if *emitIR {
			check(fmt.Errorf("-runtime-covermeta cannot be combined with -emit-ir"))
		}
		if *obj || *asm {
			check(fmt.Errorf("-runtime-covermeta requires an executable build"))
		}
		m, runtimeCoverage, err = goc.CompileExecutableWithRuntimeCoverageFor(target, filepath.Base(input), src)
	} else if buildExecutable && target == goc.TargetARM64 {
		// Only arm64 can be given the full Go runtime today; other targets get
		// the freestanding subset, which compiles a single file with no runtime.
		m, err = goc.CompileExecutableFor(target, filepath.Base(input), src)
	} else {
		m, err = goc.CompileFor(target, filepath.Base(input), src)
	}
	check(err)
	if *optimize {
		opt.OptimizeModule(m)
	}
	switch {
	case *emitIR:
		fmt.Print(m)
	case *asm:
		s, err := compileAsm(target, m)
		check(err)
		write(path(*out, input, ".s"), []byte(s))
	case *obj:
		b, err := compileObject(target, m)
		check(err)
		write(path(*out, input, ".o"), b)
	default:
		exe := *out
		if exe == "" {
			exe = goc.OutputName(input)
		}
		link(target, m, exe)
		if runtimeCoverage != nil {
			metadata, marshalErr := json.MarshalIndent(runtimeCoverage, "", "  ")
			check(marshalErr)
			metadata = append(metadata, '\n')
			check(os.WriteFile(*runtimeCoverMeta, metadata, 0o644))
		}
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
	targetName := flags.String("target", defaultTargetName(), "arm64 | amd64")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: goc test [-O] [-v] [-target arch] [-run regexp] [-o testbinary] package")
		return 2
	}
	target, err := goc.ParseTarget(*targetName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	// A test executable needs the whole Go runtime, which only arm64 can compile.
	if target != goc.TargetARM64 {
		fmt.Fprintf(os.Stderr, "goc: test executables are not supported for %s\n", target)
		return 1
	}

	packagePath := flags.Arg(0)
	module, _, err := goc.CompileTestExecutableMatchingFor(target, packagePath, *runPattern)
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
	link(target, module, executable)

	programArguments := make([]string, 0, 2)
	if *verbose {
		programArguments = append(programArguments, "-test.v=true")
	}
	if *runPattern != "" {
		programArguments = append(programArguments, "-test.run="+*runPattern)
	}
	return runProgram(executable, programArguments...)
}

// defaultTargetName is the -target default: the host, unless GOARCH names
// something else.
//
// Honoring GOARCH keeps `GOARCH=arm64 goc ...` doing what a Go programmer
// expects, and matches how the toolchain this frontend imitates picks its target.
// The flag still wins, so the env var is only ever a default. GOOS is not
// consulted, because goc.Target has no OS axis.
func defaultTargetName() string {
	if arch := os.Getenv("GOARCH"); arch != "" {
		return arch
	}
	return string(goc.HostTarget())
}

func compileAsm(target goc.Target, m *ir.Module) (string, error) {
	switch target {
	case goc.TargetAMD64:
		return "", fmt.Errorf("assembly display is not available for the object-only amd64 backend")
	case goc.TargetARM64:
		object, err := arm64.CompileToObject(m)
		if err != nil {
			return "", err
		}
		return arm64.Disassemble(object), nil
	}
	return "", fmt.Errorf("unsupported target %s", target)
}
func compileObject(target goc.Target, m *ir.Module) ([]byte, error) {
	switch target {
	case goc.TargetAMD64:
		return amd64.CompileObject(m)
	case goc.TargetARM64:
		return arm64.CompileObject(m)
	}
	return nil, fmt.Errorf("unsupported target %s", target)
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

func link(target goc.Target, m *ir.Module, exe string) {
	var translatedAssembly string
	// A temporary *directory* with fixed names inside, rather than temporary
	// files with random ones. cc records the object's filename in the linked
	// binary, so random names made two builds of the same source differ in
	// their symbol tables and defeated any comparison of the output.
	work, err := os.MkdirTemp("", "cg12-goc")
	check(err)
	defer os.RemoveAll(work)
	f, err := os.Create(filepath.Join(work, "goc.o"))
	check(err)

	// arm64 is the only backend with the assembly sidecar the Go runtime needs:
	// it emits the object plus translated Plan 9 assembly, which is then compiled
	// into a second object and linked alongside.
	if target == goc.TargetARM64 {
		translatedAssembly, err = arm64.WriteObjectAndAssembly(f, m)
	} else {
		var objectBytes []byte
		objectBytes, err = compileObject(target, m)
		if err == nil {
			_, err = f.Write(objectBytes)
		}
	}
	check(err)
	check(f.Close())
	cc, err := exec.LookPath("cc")
	check(err)
	inputs := []string{f.Name()}
	if target == goc.TargetARM64 {
		inputs = append(inputs, compileRuntimeSupport(cc, work, translatedAssembly))
	}
	args := append([]string{"-no-pie", "-o", exe}, inputs...)
	cmd := exec.Command(cc, args...)
	cmd.Stderr = os.Stderr
	check(cmd.Run())
}

func compileRuntimeSupport(cc, work, assembly string) string {
	source := filepath.Join(work, "goc-runtime.S")
	object := filepath.Join(work, "goc-runtime.o")
	check(os.WriteFile(source, []byte(assembly), 0o644))
	cmd := exec.Command(cc, "-c", "-o", object, source)
	cmd.Stderr = os.Stderr
	check(cmd.Run())
	return object
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
