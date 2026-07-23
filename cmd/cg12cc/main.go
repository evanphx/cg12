// Command cg12cc is a gcc-compatible compiler driver built on cg12, enough to
// stand in as CC for a real build system (make, autoconf). It compiles each C
// source with cg12's front end and native backend; it delegates linking (and a
// few preprocessor-only jobs it does not implement) to the host cc.
//
// Only the flags a build actually needs are acted on: -c, -o, -I, -D, -U,
// -include. The rest -- optimization levels, warnings, -f/-m tuning, -std, and so
// on -- are recognized and ignored for compilation, and forwarded verbatim when
// the driver links.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/opt"
)

type driver struct {
	compileOnly bool     // -c
	output      string   // -o
	includes    []string // -I
	defines     []string // -D
	undefines   []string // -U
	preInc      []string // -include
	sources     []string // .c inputs
	linkInputs  []string // .o/.a and non-source inputs, for the link step
	passthru    []string // flags to forward to the host cc when linking
	depTargets  []string // -MMD/-MD: also emit a .d via the host preprocessor
	depFile     string   // -MF
	verbose     bool
	optimize    bool // -O1/-O2/-O3/-Os/... (anything but -O0): run the cg12 optimizer
}

func main() {
	// cg12 compiles C, not assembly: hand any job that assembles a .s/.S source to
	// the host cc unchanged (Ruby's coroutine context switch is written in asm).
	for _, a := range os.Args[1:] {
		if strings.HasSuffix(a, ".s") || strings.HasSuffix(a, ".S") {
			os.Exit(runHostCC(os.Args[1:]))
		}
	}

	d := parseArgs(os.Args[1:])
	if len(d.sources) == 0 && !d.compileOnly {
		// Pure link (or a job with no C sources): hand it to the host cc, but force
		// a non-PIE link since cg12's objects are not position-independent.
		os.Exit(runHostCC(forceNoPIE(os.Args[1:])))
	}

	var objs []string
	for _, src := range d.sources {
		obj := d.objectNameFor(src)
		if err := d.compile(src, obj); err != nil {
			fmt.Fprintf(os.Stderr, "cg12cc: %v\n", err)
			os.Exit(1)
		}
		if d.depFile != "" || len(d.depTargets) > 0 {
			d.emitDeps(src, obj)
		}
		objs = append(objs, obj)
	}

	if d.compileOnly {
		return // objects already at their -o / derived paths
	}

	// Link: the produced objects, the object inputs, and the forwarded flags,
	// forced non-PIE (cg12's objects are not position-independent).
	args := forceNoPIE(d.passthru)
	if d.output != "" {
		args = append(args, "-o", d.output)
	}
	args = append(args, objs...)
	args = append(args, d.linkInputs...)
	os.Exit(runHostCC(args))
}

// parseArgs splits a gcc-style command line into what cg12 needs and what is
// forwarded to the host cc at link time.
func parseArgs(args []string) *driver {
	d := &driver{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "-c":
			d.compileOnly = true
		case a == "-o":
			d.output = next()
		case a == "-I":
			d.includes = append(d.includes, next())
		case strings.HasPrefix(a, "-I"):
			d.includes = append(d.includes, a[2:])
		case a == "-D":
			d.defines = append(d.defines, next())
		case strings.HasPrefix(a, "-D"):
			d.defines = append(d.defines, a[2:])
		case a == "-U":
			d.undefines = append(d.undefines, next())
		case strings.HasPrefix(a, "-U"):
			d.undefines = append(d.undefines, a[2:])
		case a == "-include":
			d.preInc = append(d.preInc, next())
		case a == "-isystem":
			d.includes = append(d.includes, next())
		case a == "-MMD" || a == "-MD":
			d.depTargets = append(d.depTargets, "") // emit deps as a side effect
		case a == "-MF":
			d.depFile = next()
		case a == "-MT" || a == "-MQ":
			d.depTargets = append(d.depTargets, next())
		case a == "-MP" || a == "-MM" || a == "-M":
			// dependency-only knobs handled by emitDeps via the host preprocessor
		case a == "-v" || a == "--verbose":
			d.verbose = true
			d.passthru = append(d.passthru, a)
		case a == "-O0":
			d.optimize = false
			d.passthru = append(d.passthru, a)
		case strings.HasPrefix(a, "-O"):
			// -O1/-O2/-O3/-Os/-Ofast/-Og: run the cg12 optimizer (also forward the
			// flag so the host cc optimizes at link/LTO if it does anything there).
			d.optimize = true
			d.passthru = append(d.passthru, a)
		case strings.HasSuffix(a, ".c"):
			d.sources = append(d.sources, a)
		case strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") ||
			strings.HasSuffix(a, ".so") || strings.HasSuffix(a, ".lo"):
			d.linkInputs = append(d.linkInputs, a)
			d.passthru = append(d.passthru, a)
		default:
			// Everything else (optimization, warnings, -f/-m tuning, -std, -g,
			// -l/-L, -Wl,..., -pthread, ...) is ignored for compiling and forwarded
			// to the host cc for linking.
			d.passthru = append(d.passthru, a)
		}
	}
	// Object inputs were already appended to passthru above; don't double them.
	d.passthru = dedupeLinkInputs(d.passthru, d.linkInputs)
	return d
}

// dedupeLinkInputs removes the object inputs from passthru (they are re-added in
// link order after the compiled objects), keeping every other forwarded flag.
func dedupeLinkInputs(passthru, linkInputs []string) []string {
	drop := map[string]bool{}
	for _, o := range linkInputs {
		drop[o] = true
	}
	var out []string
	for _, a := range passthru {
		if drop[a] {
			continue
		}
		out = append(out, a)
	}
	return out
}

// objectNameFor is where a compiled source goes: -o if compiling a single source,
// else the source's basename with a .o suffix.
func (d *driver) objectNameFor(src string) string {
	if d.compileOnly && d.output != "" && len(d.sources) == 1 {
		return d.output
	}
	base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	return base + ".o"
}

// compile turns one C source into a native object with cg12.
func (d *driver) compile(src, obj string) error {
	text, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if d.verbose {
		fmt.Fprintf(os.Stderr, "cg12cc: compiling %s -> %s\n", src, obj)
	}
	mod, err := cc.CompileWith(src, string(text), cc.Options{
		Target:      cc.TargetARM64,
		IncludeDirs: d.includes,
		Defines:     d.defines,
		Undefines:   d.undefines,
		PreIncludes: d.preInc,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	if d.optimize {
		opt.OptimizeModule(mod)
	}
	code, err := arm64.CompileObject(mod)
	if err != nil {
		return fmt.Errorf("%s: codegen: %w", src, err)
	}
	return os.WriteFile(obj, code, 0o644)
}

// emitDeps produces a make dependency file with the host preprocessor, since cg12
// does not track includes itself. A build's .d files are an optimization; if this
// fails it is not fatal.
func (d *driver) emitDeps(src, obj string) {
	target := obj
	if len(d.depTargets) > 0 && d.depTargets[len(d.depTargets)-1] != "" {
		target = d.depTargets[len(d.depTargets)-1]
	}
	out := d.depFile
	if out == "" {
		out = strings.TrimSuffix(obj, filepath.Ext(obj)) + ".d"
	}
	args := []string{"-MM", "-MT", target}
	for _, inc := range d.includes {
		args = append(args, "-I", inc)
	}
	for _, def := range d.defines {
		args = append(args, "-D", def)
	}
	args = append(args, src, "-o", out)
	_ = exec.Command(hostCC(), args...).Run()
}

// runHostCC runs the host C compiler (the linker step, or a job cg12 does not do)
// and returns its exit code.
func runHostCC(args []string) int {
	cmd := exec.Command(hostCC(), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "cg12cc: host cc: %v\n", err)
		return 1
	}
	return 0
}

// forceNoPIE drops position-independent link/codegen flags and prepends -no-pie,
// because cg12's objects reach external data with absolute/PC-relative
// relocations rather than through the GOT, which a PIE link rejects.
func forceNoPIE(args []string) []string {
	out := []string{"-no-pie"}
	for _, a := range args {
		switch a {
		case "-pie", "-fPIE", "-fpie", "-fPIC", "-fpic", "-shared":
			continue
		}
		out = append(out, a)
	}
	return out
}

func hostCC() string {
	if cc := os.Getenv("CG12_HOST_CC"); cc != "" {
		return cc
	}
	return "cc"
}
