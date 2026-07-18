// Command cg12 is a small, composable toolkit for cg12 IR: parse text to the
// binary unit format, optimize, assemble to machine code, and interpret —
// pieces that pipe over stdin/stdout.
//
//	cg12 parse f.ssa             # text IL -> binary IR
//	cg12 print f.irb             # IR (binary or text) -> text IL
//	cg12 opt f.irb               # IR -> optimized binary IR
//	cg12 opt -S f.ssa            # ... written as text
//	cg12 asm -o f.o f.irb        # IR -> arm64 object
//	cg12 asm -target wasm f.irb  # IR -> wasm text
//	cg12 asm -S f.irb            # IR -> assembly text
//	cg12 run -fn f f.irb 3 4     # interpret f(3, 4)
//
// Every command that reads IR auto-detects binary vs text; commands that write
// IR default to the binary unit and take -S to write text instead. Input is a
// file argument or stdin ("-"), output is -o or stdout, so the tools pipe:
//
//	cg12 parse f.ssa | cg12 opt | cg12 asm -o f.o
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/bpf"
	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/parse"
	"github.com/evanphx/cg12/wasm"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "parse":
		cmdParse(args)
	case "print":
		cmdPrint(args)
	case "opt":
		cmdOpt(args)
	case "asm":
		cmdAsm(args)
	case "run":
		cmdRun(args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "cg12: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: cg12 <command> [flags] [file]

  parse   text IL -> binary IR unit
  print   IR (binary or text) -> text IL
  opt     IR -> optimized IR        (-S for text)
  asm     IR -> object code         (-target arm64|amd64|wasm|bpf, -S for assembly text)
  run     interpret IR: call a function (-fn name, args follow the file)

IR input is auto-detected as the binary unit or text; a file argument or "-"
(stdin) is read, and -o (or stdout) is written.
`)
}

// cmdParse turns text IL into the binary unit. (Binary input passes through.)
func cmdParse(args []string) {
	fs := flag.NewFlagSet("parse", flag.ExitOnError)
	out := fs.String("o", "", "output file (default stdout)")
	fs.Parse(args)
	writeIR(*out, readModule(fs.Arg(0)), false)
}

// cmdPrint renders IR as text, whatever form it came in.
func cmdPrint(args []string) {
	fs := flag.NewFlagSet("print", flag.ExitOnError)
	out := fs.String("o", "", "output file (default stdout)")
	fs.Parse(args)
	writeBytes(*out, []byte(readModule(fs.Arg(0)).String()))
}

// cmdOpt runs the optimizer.
func cmdOpt(args []string) {
	fs := flag.NewFlagSet("opt", flag.ExitOnError)
	out := fs.String("o", "", "output file (default stdout)")
	text := fs.Bool("S", false, "write text IL instead of the binary unit")
	fs.Parse(args)
	m := readModule(fs.Arg(0))
	opt.OptimizeModule(m)
	writeIR(*out, m, *text)
}

// cmdAsm compiles IR to machine code for a target.
func cmdAsm(args []string) {
	fs := flag.NewFlagSet("asm", flag.ExitOnError)
	out := fs.String("o", "", "output file (default stdout)")
	target := fs.String("target", "arm64", "arm64 | amd64 | wasm | bpf")
	text := fs.Bool("S", false, "write assembly text instead of an object")
	fs.Parse(args)
	m := readModule(fs.Arg(0))

	switch *target {
	case "arm64":
		if *text {
			o, err := arm64.CompileToObject(m)
			check(err)
			writeBytes(*out, []byte(arm64.Disassemble(o)))
			return
		}
		data, err := arm64.CompileObject(m)
		check(err)
		writeBytes(*out, data)
	case "amd64":
		if *text {
			fatalf("assembly text (-S) is not available for amd64")
		}
		data, err := amd64.CompileObject(m)
		check(err)
		writeBytes(*out, data)
	case "wasm":
		src, err := wasm.CompileModule(m) // wasm's object form is the .wat text
		check(err)
		writeBytes(*out, []byte(src))
	case "bpf":
		o, err := bpf.CompileModule(m)
		check(err)
		if *text {
			var b []byte
			for _, p := range o.Programs {
				b = append(b, []byte(fmt.Sprintf("// %s [%s]\n%s", p.Name, p.Section, p.Asm()))...)
			}
			writeBytes(*out, b)
			return
		}
		writeBytes(*out, o.ELF())
	default:
		fatalf("unknown target %q (arm64 | amd64 | wasm | bpf)", *target)
	}
}

// cmdRun interprets the IR, calling a function with integer arguments.
func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fn := fs.String("fn", "main", "function to call")
	optimize := fs.Bool("O", false, "optimize before running")
	fs.Parse(args)

	m := readModule(fs.Arg(0))
	if *optimize {
		opt.OptimizeModule(m)
	}
	mc, err := interp.New(m)
	check(err)

	var callArgs []interp.Value
	for _, a := range fs.Args()[1:] {
		n, err := strconv.ParseInt(a, 0, 64)
		if err != nil {
			fatalf("argument %q is not an integer", a)
		}
		callArgs = append(callArgs, interp.L(n))
	}
	v, err := mc.Call(*fn, callArgs...)
	check(err)
	fmt.Println(v.I64())
}

// --- IO + module helpers ---------------------------------------------------

const binMagic = "cg12" // the first bytes of a binary unit (ir.Module.MarshalBinary)

// readModule reads a module from a file (or stdin for "" / "-").
func readModule(path string) *ir.Module {
	m, err := decodeModule(readInput(path))
	check(err)
	return m
}

// decodeModule turns the bytes of a unit into a module, auto-detecting the binary
// unit versus text IL by its leading bytes.
func decodeModule(data []byte) (*ir.Module, error) {
	if len(data) >= len(binMagic) && string(data[:len(binMagic)]) == binMagic {
		return ir.DecodeModule(data)
	}
	return parse.Parse(string(data))
}

func readInput(path string) []byte {
	if path == "" || path == "-" {
		data, err := io.ReadAll(os.Stdin)
		check(err)
		return data
	}
	data, err := os.ReadFile(path)
	check(err)
	return data
}

func writeIR(path string, m *ir.Module, text bool) {
	if text {
		writeBytes(path, []byte(m.String()))
		return
	}
	data, err := m.MarshalBinary()
	check(err)
	writeBytes(path, data)
}

func writeBytes(path string, data []byte) {
	if path == "" || path == "-" {
		_, err := os.Stdout.Write(data)
		check(err)
		return
	}
	check(os.WriteFile(path, data, 0o644))
}

func check(err error) {
	if err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "cg12: "+format+"\n", a...)
	os.Exit(1)
}
