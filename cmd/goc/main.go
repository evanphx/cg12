// Command goc compiles a Go source file through the cg12 backend.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	if len(os.Args) > 1 && os.Args[1] == "compile-batch" {
		os.Exit(compileBatchCommand(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}

	out := flag.String("o", "", "output file")
	obj := flag.Bool("c", false, "emit a relocatable object")
	asm := flag.Bool("S", false, "emit assembly")
	emitIR := flag.Bool("emit-ir", false, "print cg12 IR")
	optimize := flag.Bool("O", false, "optimize cg12 IR")
	run := flag.Bool("run", false, "link and run the program")
	runtimeCoverMeta := flag.String("runtime-covermeta", "", "instrument runtime and write coverage metadata")
	prebuiltRuntime := flag.String("runtime", "", "link against the prebuilt runtimes written by `goc build-runtime`, comma-separated; the richest usable one is chosen")
	cpuProfile := flag.String("cpuprofile", "", "write a CPU profile of the compile to this file")
	memProfile := flag.String("memprofile", "", "write a heap profile of the compile to this file")
	memProfilePeak := flag.Bool("memprofile-peak", false, "with -memprofile, profile the compile's peak heap instead of what it retains at the end")
	targetName := flag.String("target", defaultTargetName(), "arm64 | amd64")
	var escapeDiagLevel escapeDiagFlag
	flag.Var(&escapeDiagLevel, "m", "print escape decisions: 1 the rule that placed each allocation, 2 also the chain to the deciding use")
	escapeDiag := (*int)(&escapeDiagLevel)
	escapeDiagMatch := flag.String("m-match", "", "with -m, report every source path containing this `substring` instead of only the compiled file")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: goc [-O] [-m[=2]] [-m-match pkg] [-target arch] [-o out] [-runtime runtime.gocrt] [-cpuprofile prof] [-c|-S|-emit-ir|-run] file.go")
		os.Exit(2)
	}
	// -m overrides GOC_M, which opt read at init. Off is the default in both, and
	// with it off nothing below behaves differently in any way -- see
	// docs/escape-diagnostics.md.
	if *escapeDiag > 0 {
		opt.SetEscapeDiagLevel(*escapeDiag)
	}
	// Likewise for -m-match over GOC_M_MATCH. An empty value is the default
	// restriction to the compiled file, so an unset flag leaves the environment's
	// answer alone rather than clearing it.
	if *escapeDiagMatch != "" {
		opt.SetEscapeDiagMatch(*escapeDiagMatch)
	}
	if *cpuProfile != "" {
		check(startCPUProfile(*cpuProfile))
		defer stopCPUProfile()
	}
	// The heap profile covers the same span as the CPU one -- the compile, not
	// the link and not the compiled program -- so it is taken where the module is
	// finished rather than at process exit. finishMemProfile is what marks that
	// point, and every path that reaches a finished module calls it.
	var peakProfiler *peakHeapProfiler
	if *memProfile != "" && *memProfilePeak {
		started, err := startPeakHeapProfile(*memProfile)
		check(err)
		peakProfiler = started
		defer peakProfiler.Stop()
	}
	finishMemProfile := func() {
		if *memProfile == "" {
			return
		}
		if peakProfiler != nil {
			peakProfiler.Stop()
			peakProfiler = nil
			return
		}
		check(writeHeapProfile(*memProfile, true))
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
		packs, err := readPackSet(splitCommaList(*prebuiltRuntime))
		check(err)
		check(linkAgainstPrebuiltRuntime(target, packs, input, src, exe, *optimize, os.Stderr))
		finishMemProfile()
		if *run {
			// The profile covers the compile, not the compiled program, and
			// os.Exit does not run deferred functions.
			stopCPUProfile()
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
	finishMemProfile()
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
			stopCPUProfile()
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
	check(linkModule(target, m, exe, os.Stderr))
}

// linkModule emits a finished module and links it into an executable, sending
// everything the C toolchain says to errorOutput.
//
// It takes the writer rather than using os.Stderr because the batch compiler
// runs many of these in one process and has to attribute each linker complaint
// to the program that caused it; the command-line path passes os.Stderr and is
// unchanged.
func linkModule(target goc.Target, m *ir.Module, exe string, errorOutput io.Writer) error {
	var translatedAssembly string
	// A temporary *directory* with fixed names inside, rather than temporary
	// files with random ones. cc records the object's filename in the linked
	// binary, so random names made two builds of the same source differ in
	// their symbol tables and defeated any comparison of the output.
	work, err := os.MkdirTemp("", "cg12-goc")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	f, err := os.Create(filepath.Join(work, "goc.o"))
	if err != nil {
		return err
	}

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
	if err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		return err
	}
	inputs := []string{f.Name()}
	if target == goc.TargetARM64 {
		supportObject, err := compileRuntimeSupport(cc, work, translatedAssembly, errorOutput)
		if err != nil {
			return err
		}
		inputs = append(inputs, supportObject)
	}
	args := append([]string{"-no-pie", "-o", exe}, inputs...)
	cmd := exec.Command(cc, args...)
	cmd.Stderr = errorOutput
	return cmd.Run()
}

func compileRuntimeSupport(cc, work, assembly string, errorOutput io.Writer) (string, error) {
	source := filepath.Join(work, "goc-runtime.S")
	object := filepath.Join(work, "goc-runtime.o")
	if err := os.WriteFile(source, []byte(assembly), 0o644); err != nil {
		return "", err
	}
	cmd := exec.Command(cc, "-c", "-o", object, source)
	cmd.Stderr = errorOutput
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return object, nil
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

// escapeDiagFlag makes -m behave the way gc's does: bare -m means level 1, and
// -m=2 asks for the chain. A plain flag.Int cannot do this -- `goc -m file.go`
// tries to parse the filename as the level and fails -- so this reports itself
// as a boolean flag, which is what lets the flag package accept the bare form.
type escapeDiagFlag int

func (level *escapeDiagFlag) String() string { return strconv.Itoa(int(*level)) }

func (level *escapeDiagFlag) Set(value string) error {
	if value == "true" {
		*level = 1
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("escape diagnostic level: %w", err)
	}
	*level = escapeDiagFlag(parsed)
	return nil
}

// IsBoolFlag lets `-m` stand alone. The flag package then passes "true" to Set.
func (level *escapeDiagFlag) IsBoolFlag() bool { return true }
