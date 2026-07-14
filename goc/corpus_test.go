package goc_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

// TestExecutionCorpus climbs from expressions to stateful control flow. Every
// case passes through the Go parser/type checker, cg12 IR, native machine-code
// emitter, ELF writer, system linker, and finally the host CPU.
func TestExecutionCorpus(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"constants", `func Test() int { return 42 }`, 42},
		{"arithmetic precedence", `func Test() int { return 2 + 3*4 - 8/2 }`, 10},
		{"signed division and remainder", `func Test() int { return (-17/5)*10 + (-17%5) }`, -32},
		{"unsigned arithmetic", `func Test() int { var x uint = 1<<63; return int((x>>62) + x/x) }`, 3},
		{"bitwise", `func Test() int { return (0x55 & 0x0f) | (3 << 4) ^ 2 }`, 55},
		{"signed byte overflow", `func Test() int { var x int8=127; x++; return int(x) }`, -128},
		{"unsigned byte overflow", `func Test() int { var x uint8=255; x+=2; return int(x) }`, 1},
		{"word overflow", `func Test() int { var x uint32=0xffffffff; x++; return int(x) }`, 0},
		{"comparisons", `func Test() int { n:=0; if -1 < 1 { n+=1 }; if uint(1) < uint(2) { n+=2 }; if 3 != 4 { n+=4 }; return n }`, 7},
		{"locals and compound assignment", `func Test() int { x:=3; x+=4; x*=5; x-=2; return x }`, 33},
		{"parallel assignment", `func Test() int { x,y:=3,8; x,y=y,x; return x*10+y }`, 83},
		{"lexical shadowing", `func Test() int { x:=2; { x:=9; x++ }; return x }`, 2},
		{"if init and else", `func Test() int { if x:=7; x>8 { return 1 } else if x==7 { return 2 }; return 3 }`, 2},
		{"for loop", `func Test() int { s:=0; for i:=1; i<=10; i++ { s+=i }; return s }`, 55},
		{"break and continue", `func Test() int { s:=0; for i:=0; ; i++ { if i==8 { break }; if i%2==0 { continue }; s+=i }; return s }`, 16},
		{"function call", `func twice(x int) int { return x*2 }; func Test() int { return twice(21) }`, 42},
		{"recursion", `func fib(n int) int { if n<2 { return n }; return fib(n-1)+fib(n-2) }; func Test() int { return fib(10) }`, 55},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, optimized := range []bool{false, true} {
				name := "unoptimized"
				if optimized {
					name = "optimized"
				}
				t.Run(name, func(t *testing.T) { runCase(t, "package main\n"+tc.body, tc.want, optimized) })
			}
		})
	}
}

func TestAdvancedExecutionCorpus(t *testing.T) {
	cases := []struct {
		name, body string
		want       int
	}{
		{"short circuit and", `var calls int; func yes() bool { calls++; return true }; func Test() int { if false && yes() {}; return calls }`, 0},
		{"short circuit or", `var calls int; func yes() bool { calls++; return true }; func Test() int { if true || yes() {}; return calls }`, 0},
		{"switch", `func Test() int { x:=3; switch x { case 1: return 10; case 2,3: return 20; default: return 30 } }`, 20},
		{"switch fallthrough", `func Test() int { n:=0; switch 2 { case 2: n+=2; fallthrough; case 3: n+=3 }; return n }`, 5},
		{"package constants", `const base=40; const two int=2; func Test() int { return base+two }`, 42},
		{"mutable globals", `var total=3; func add(x int){ total+=x }; func Test() int { add(4); add(5); return total }`, 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runCase(t, "package main\n"+tc.body, tc.want, false) })
	}
}

func TestStandardLibrarySHA256(t *testing.T) {
	runCase(t, `package main

import "crypto/sha256"

func Test() int {
	sum := sha256.Sum256([]byte("abc"))
	fingerprint := 0
	for i := 0; i < len(sum); i++ {
		fingerprint = (fingerprint*257 + int(sum[i])) % 2147483647
	}
	return fingerprint
}
`, 739054043, false)
}

func TestRepositoryStandardLibraryUTF8(t *testing.T) {
	runCase(t, `package main

import "unicode/utf8"

func Test() int {
	return utf8.RuneLen('世')
}
`, 3, false)
}

func runCase(t *testing.T, src string, want int, optimized bool) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("ELF execution test")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("system C linker unavailable")
	}
	m, err := goc.Compile("case.go", []byte(src))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if optimized {
		opt.OptimizeModule(m)
	}
	b, err := nativeObject(m)
	if err != nil {
		t.Fatalf("machine code: %v", err)
	}
	d := t.TempDir()
	obj := filepath.Join(d, "case.o")
	if err := os.WriteFile(obj, b, 0o644); err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(d, "harness.c")
	csrc := fmt.Sprintf("#include <stdio.h>\nextern long main_Test(void); int main(void) { long got=main_Test(); if (got != %d) fprintf(stderr, \"got %%ld\\n\", got); return got == %d ? 0 : 1; }\n", want, want)
	if err := os.WriteFile(harness, []byte(csrc), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(d, "case")
	cmd := exec.Command(cc, "-o", exe, harness, obj)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("link: %v\n%s", err, out)
	}
	if out, err := exec.Command(exe).CombinedOutput(); err != nil {
		t.Fatalf("result != %d: %v\n%s", want, err, out)
	}
}

func nativeObject(m *ir.Module) ([]byte, error) {
	switch runtime.GOARCH {
	case "amd64":
		return amd64.CompileObject(m)
	case "arm64":
		return arm64.CompileObject(m)
	default:
		return nil, fmt.Errorf("unsupported architecture %s", runtime.GOARCH)
	}
}
