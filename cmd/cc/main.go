// Command cc is a tiny C compiler driver built on cg12: it compiles a single .c
// file to a native (arm64) executable or object, or runs it directly.
//
//	cc hello.c            # compile + link -> ./hello
//	cc -o prog hello.c    # ... to ./prog
//	cc -run hello.c       # compile, link, run (exit code propagates)
//	cc -c hello.c         # -> hello.o (relocatable object)
//	cc -dis hello.c       # print the generated code, read back out of the object
//	cc -emit-ir hello.c   # print the cg12 IR to stdout
//	cc -O -emit-ir hello.c # ... after running the optimizer
//	cc -O -bpf prog.c     # compile to eBPF and print the disassembly
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/bpf"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

func main() {
	out := flag.String("o", "", "output file (default derived from the input name)")
	emitObj := flag.Bool("c", false, "compile to a relocatable object (.o), do not link")
	emitDis := flag.Bool("dis", false, "print the generated code, read back out of the object")
	emitIR := flag.Bool("emit-ir", false, "print the cg12 IR to stdout")
	emitBPF := flag.Bool("bpf", false, "compile each function to eBPF and print the disassembly")
	emitBPFELF := flag.Bool("bpf-elf", false, "compile to a loadable eBPF ELF object (.bpf.o)")
	run := flag.Bool("run", false, "compile, link, and run; propagate the exit code")
	optimize := flag.Bool("O", false, "run the cg12 optimizer before code generation")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 1 {
		usage()
		os.Exit(2)
	}
	input := flag.Arg(0)

	src, err := os.ReadFile(input)
	check(err)

	mod, err := cc.Compile(filepath.Base(input), string(src))
	check(err)

	if *optimize {
		opt.OptimizeModule(mod)
	}

	switch {
	case *emitIR:
		fmt.Print(mod.String())
	case *emitBPF:
		obj, err := bpf.CompileModule(mod)
		check(err)
		for _, m := range obj.Maps {
			fmt.Printf("// map %s: type=%d key=%d value=%d max_entries=%d\n", m.Name, m.Type, m.KeySize, m.ValueSize, m.MaxEntries)
		}
		for _, p := range obj.Programs {
			sec := p.Section
			if sec == "" {
				sec = "(no section)"
			}
			fmt.Printf("// %s [%s]: %d eBPF instructions\n%s", p.Name, sec, len(p.Insns), p.Asm())
		}
	case *emitBPFELF:
		obj, err := bpf.CompileModule(mod)
		check(err)
		writeOut(outputPath(*out, input, ".bpf.o"), obj.ELF())
	case *emitDis:
		o, err := arm64.CompileToObject(mod)
		check(err)
		fmt.Print(arm64.Disassemble(o))
	case *emitObj:
		writeObject(mod, outputPath(*out, input, ".o"))
	default:
		exe, cleanup := outputExe(*out, input, *run)
		defer cleanup()
		link(mod, exe)
		if *run {
			code := runProg(exe)
			cleanup() // os.Exit runs no deferred calls
			os.Exit(code)
		}
	}
}

// outputExe decides where the linked executable goes. With -o it is the caller's
// choice. Without one, a plain compile names it after the input, the way cc does.
// For -run, though, the executable is a means rather than something asked for, so
// it goes somewhere temporary and is taken away again -- as go run and tcc -run
// behave. Naming it after the input and leaving it in the working directory
// litters the caller's tree with something they never asked to keep.
func outputExe(out, input string, run bool) (path string, cleanup func()) {
	if out != "" {
		return out, func() {}
	}
	name := strings.TrimSuffix(filepath.Base(input), filepath.Ext(input))
	if !run {
		return name, func() {}
	}
	dir, err := os.MkdirTemp("", "cg12cc-*")
	check(err)
	return filepath.Join(dir, name), func() { os.RemoveAll(dir) }
}

// writeObject compiles the module to an ELF object and writes it.
func writeObject(mod *ir.Module, path string) {
	code, err := arm64.CompileObject(mod)
	check(err)
	writeOut(path, code)
}

// link compiles to a temporary object and links it into an executable with the
// native toolchain (so libc is available).
func link(mod *ir.Module, exe string) {
	code, err := arm64.CompileObject(mod)
	check(err)
	tmp, err := os.CreateTemp("", "cg12cc-*.o")
	check(err)
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(code)
	check(err)
	check(tmp.Close())

	cc := lookupCC()
	cmd := exec.Command(cc, "-o", exe, tmp.Name())
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatalf("link failed: %v", err)
	}
}

// runProg runs the built executable and returns its exit code.
func runProg(exe string) int {
	abs, err := filepath.Abs(exe)
	check(err)
	cmd := exec.Command(abs)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	check(err)
	return 0
}

func lookupCC() string {
	for _, name := range []string{"cc", "gcc", "clang"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	fatalf("no C toolchain (cc/gcc/clang) found for linking")
	return ""
}

// outputPath returns the explicit -o value, or the input's base name with ext.
func outputPath(out, input, ext string) string {
	if out != "" {
		return out
	}
	base := strings.TrimSuffix(filepath.Base(input), filepath.Ext(input))
	return base + ext
}

func writeOut(path string, data []byte) {
	check(os.WriteFile(path, data, 0o644))
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: cc [-O] [-o out] [-c|-dis|-emit-ir|-run] file.c\n")
	flag.PrintDefaults()
}

func check(err error) {
	if err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "cc: "+format+"\n", a...)
	os.Exit(1)
}
