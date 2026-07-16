package link_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/link"
	"github.com/stretchr/testify/require"
)

// runExe writes the linked bytes to a temp file, marks it executable, runs it
// (directly, or through runner such as qemu), and returns the process exit code.
// A process killed by a signal reports 128+signal, as a shell does, so a crash is
// a legible number rather than the -1 that ExitCode gives for every signal alike.
func runExe(t *testing.T, data []byte, runner ...string) int {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.out")
	require.NoError(t, os.WriteFile(path, data, 0o755))
	argv := append(append([]string{}, runner...), path)
	err := exec.Command(argv[0], argv[1:]...).Run()
	if err == nil {
		return 0
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run %v: %v", argv, err)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ee.ExitCode()
}

// qemuX86 returns the qemu-x86_64 user-mode emulator, or skips.
func qemuX86(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("qemu-x86_64")
	if err != nil {
		t.Skip("qemu-x86_64 not available")
	}
	return p
}

// moduleReturns builds a function that returns the constant v.
func moduleReturns(name string, v int64) *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc(name, ir.ClsW).Export()
	f.Entry().Ret(f.Word(v))
	return m
}

// moduleMainCallsHelper builds main() = helper() + 1, referencing an external
// helper defined in another module.
func moduleMainCallsHelper() *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, e.Call(ir.ClsW, f.Sym("helper", 0)), f.Word(1)))
	return m
}

// moduleReadsGlobal builds main() = *g, where g is an initialized module-level
// word. Addressing the global exercises the symbol-address relocations the
// executable writer must resolve (arm64 ADRP+ADD, amd64 RIP-relative PC32).
func moduleReadsGlobal(v int64) *ir.Module {
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{
		Name:  "g",
		Align: 4,
		Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{v}}},
	})
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Ret(e.Load(ir.ClsW, f.Sym("g", 0)))
	return m
}

// A program that reads an initialized global links into a runnable executable:
// the symbol-address relocations are resolved against the .data segment's final
// virtual address, and the program returns the global's value.
func TestExecutableReadsGlobal(t *testing.T) {
	t.Run("arm64", func(t *testing.T) {
		if runtime.GOARCH != "arm64" {
			t.Skip("arm64 executable runs natively only on an arm64 host")
		}
		l := link.NewWith(arm64.Backend{})
		require.NoError(t, l.AddModule(moduleReadsGlobal(99)))
		exe, err := l.LinkExecutable("main")
		require.NoError(t, err)
		require.Equal(t, 99, runExe(t, exe))
	})
	t.Run("amd64", func(t *testing.T) {
		qemu := qemuX86(t)
		l := link.NewWith(amd64.Backend{})
		require.NoError(t, l.AddModule(moduleReadsGlobal(99)))
		exe, err := l.LinkExecutable("main")
		require.NoError(t, err)
		require.Equal(t, 99, runExe(t, exe, qemu))
	})
}

// The linker turns an IR module straight into a runnable static executable -- no
// external assembler, linker, or C runtime -- and the process exit code is the
// entry function's return value.
func TestExecutableReturnsConstant(t *testing.T) {
	t.Run("arm64", func(t *testing.T) {
		if runtime.GOARCH != "arm64" {
			t.Skip("arm64 executable runs natively only on an arm64 host")
		}
		l := link.NewWith(arm64.Backend{})
		require.NoError(t, l.AddModule(moduleReturns("main", 42)))
		exe, err := l.LinkExecutable("main")
		require.NoError(t, err)
		require.Equal(t, 42, runExe(t, exe))
	})
	t.Run("amd64", func(t *testing.T) {
		qemu := qemuX86(t)
		l := link.NewWith(amd64.Backend{})
		require.NoError(t, l.AddModule(moduleReturns("main", 42)))
		exe, err := l.LinkExecutable("main")
		require.NoError(t, err)
		require.Equal(t, 42, runExe(t, exe, qemu))
	})
}

// Linking an executable whose entry references an undefined symbol is an error,
// not a broken binary: a static executable cannot leave a symbol unresolved.
func TestExecutableUndefinedSymbolErrors(t *testing.T) {
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(moduleMainCallsHelper())) // main calls helper, never provided
	_, err := l.LinkExecutable("main")
	require.Error(t, err)
	require.Contains(t, err.Error(), "helper")
}

// A cross-module call is resolved into the final executable: main() calls helper()
// in another module, and the running program returns 42.
func TestExecutableCrossModuleCall(t *testing.T) {
	build := func(be link.Backend) []byte {
		l := link.NewWith(be)
		require.NoError(t, l.AddModule(moduleHelper()))          // helper() = 41
		require.NoError(t, l.AddModule(moduleMainCallsHelper())) // main() = helper() + 1
		exe, err := l.LinkExecutable("main")
		require.NoError(t, err)
		return exe
	}
	t.Run("arm64", func(t *testing.T) {
		if runtime.GOARCH != "arm64" {
			t.Skip("arm64 executable runs natively only on an arm64 host")
		}
		require.Equal(t, 42, runExe(t, build(arm64.Backend{})))
	})
	t.Run("amd64", func(t *testing.T) {
		qemu := qemuX86(t)
		require.Equal(t, 42, runExe(t, build(amd64.Backend{}), qemu))
	})
}
