//go:build linux && (amd64 || arm64)

package jit

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
)

// TestLoadAndCall loads code that calls between functions and reads a
// Float constant, which exercises the call and data relocations the
// backends emit, and checks the code's pages are never writable.
func TestLoadAndCall(t *testing.T) {
	m := ir.NewModule()
	h := m.NewFunc("half", ir.ClsL)
	x := h.Param("x", ir.ClsL)
	he := h.Entry()
	he.Ret(he.Sar(ir.ClsL, x, h.Long(1)))

	f := m.NewFunc("f", ir.ClsL).Export()
	a := f.Param("a", ir.ClsL)
	b := f.Param("b", ir.ClsL)
	c := f.Param("c", ir.ClsL)
	e := f.Entry()
	sum := e.Add(ir.ClsL, e.Call(ir.ClsL, f.Sym("half", 0), a), b)
	scaled := e.Mul(ir.ClsD, e.Sltof(ir.ClsD, sum), f.Double(2.5))
	e.Ret(e.Add(ir.ClsL, e.Cast(ir.ClsL, scaled), c))

	o, err := Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	code, err := Load(o, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer code.Free()
	got := Call(code.Addr("f"), 40, 4, 1)
	if want := math.Float64bits(2.5*24) + 1; got != want {
		t.Fatalf("f(40, 4, 1) = %#x, want %#x", got, want)
	}
	if perms := mapping(t, code.Addr("f")); perms != "r-xp" {
		t.Fatalf("the code is mapped %s, want r-xp", perms)
	}
}

// TestLoadUndefined refuses an object that refers to a symbol nobody
// defines.
func TestLoadUndefined(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("f", ir.ClsL).Export()
	e := f.Entry()
	e.Ret(e.Call(ir.ClsL, f.Sym("missing", 0)))
	o, err := Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(o, nil); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("got %v, want an undefined-symbol error", err)
	}
}

// mapping returns the permissions of the mapping that holds addr.
func mapping(t *testing.T, addr uintptr) string {
	f, err := os.Open("/proc/self/maps")
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		var lo, hi uintptr
		var perms string
		if _, err := fmt.Sscanf(s.Text(), "%x-%x %s", &lo, &hi, &perms); err == nil && lo <= addr && addr < hi {
			return perms
		}
	}
	t.Fatalf("no mapping holds %#x", addr)
	return ""
}
