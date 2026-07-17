package link_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/internal/testenv"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/link"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/require"
)

// The amd64 dynamic path was asserted only structurally: that ET_EXEC was set,
// that PT_INTERP held the right string, that PT_DYNAMIC was present. None of
// that can tell a correct PLT stub from a wrong one -- they are hand-encoded
// bytes, and a structural check reads the segment, not the instructions in it.
// So plt0Stub, pltStub, pltGotInit and the GOT initialisation had never once
// been executed.
//
// Running one needs the target's loader, which an arm64 machine has no reason to
// have; testenv.X86Sysroot finds one when there is one.

// runX86Dyn runs a dynamically linked x86-64 image under qemu with the sysroot's
// loader, returning its exit code.
func runX86Dyn(t *testing.T, image []byte) int {
	t.Helper()
	qemu := testenv.Tool(t, "qemu-x86_64")
	root := testenv.X86Sysroot(t)

	bin := filepath.Join(t.TempDir(), "prog")
	require.NoError(t, os.WriteFile(bin, image, 0o755))
	cmd := exec.Command(qemu, "-L", root, bin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		t.Fatalf("run: %v\n%s", err, out)
	}
	return 0
}

// absModule calls libc's abs through the PLT and returns |−42| − 42, so 0 says
// the call reached the real function and came back.
func absModule() *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	r := e.Call(ir.ClsW, f.Sym("abs", 0), f.Word(-42))
	e.Ret(e.Sub(ir.ClsW, r, f.Word(42)))
	return m
}

func TestAmd64DynamicExecutableRuns(t *testing.T) {
	l := link.NewWith(amd64.Backend{})
	require.NoError(t, l.AddModule(absModule()))
	exe, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{Needed: []string{"libc.so.6"}})
	require.NoError(t, err)
	require.Equal(t, 0, runX86Dyn(t, exe), "the call through the PLT reaches libc's abs")
}

// Lazy binding leaves the GOT slot pointing back into the PLT, so the first call
// runs plt0's push-and-jump into the loader's resolver and the slot is rewritten
// underneath. Eager binding resolves everything up front and never runs plt0.
// The two are different code paths through the same stubs.
func TestAmd64DynamicBindingModes(t *testing.T) {
	for _, lazy := range []bool{false, true} {
		name := "eager"
		if lazy {
			name = "lazy"
		}
		t.Run(name, func(t *testing.T) {
			l := link.NewWith(amd64.Backend{})
			require.NoError(t, l.AddModule(absModule()))
			exe, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{
				Needed: []string{"libc.so.6"}, Lazy: lazy,
			})
			require.NoError(t, err)
			require.Equal(t, 0, runX86Dyn(t, exe))
		})
	}
}

// A PIE is the same image with every absolute address turned into a relocation
// the loader rebases, so it runs from wherever it is placed.
func TestAmd64PIERuns(t *testing.T) {
	l := link.NewWith(amd64.Backend{})
	require.NoError(t, l.AddModule(absModule()))
	exe, err := l.LinkPIE("main", "libc.so.6")
	require.NoError(t, err)
	require.Equal(t, 0, runX86Dyn(t, exe), "a PIE runs wherever the loader puts it")
}

// A shared library of ours, loaded by the real loader and called from an
// executable of ours: both halves of the dynamic machinery at once.
func TestAmd64SharedLibraryRuns(t *testing.T) {
	dir := t.TempDir()

	lib := ir.NewModule()
	g := lib.NewFunc("lib_answer", ir.ClsW).Export()
	g.Entry().Ret(g.Word(42))

	ll := link.NewWith(amd64.Backend{})
	require.NoError(t, ll.AddModule(lib))
	so, err := ll.LinkSharedLibraryWith(obj.SharedOptions{
		Soname: "libanswer.so", Export: []string{"lib_answer"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "libanswer.so"), so, 0o755))

	p := ir.NewModule()
	f := p.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Ret(e.Sub(ir.ClsW, e.Call(ir.ClsW, f.Sym("lib_answer", 0)), f.Word(42)))

	l := link.NewWith(amd64.Backend{})
	require.NoError(t, l.AddModule(p))
	exe, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{
		Needed: []string{"libanswer.so", "libc.so.6"}, Runpath: []string{"$ORIGIN"},
	})
	require.NoError(t, err)

	qemu := testenv.Tool(t, "qemu-x86_64")
	root := testenv.X86Sysroot(t)
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(bin, exe, 0o755))
	cmd := exec.Command(qemu, "-L", root, bin)
	cmd.Dir = dir // so $ORIGIN finds the library
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "our library, our executable, the real loader: %s", out)
}

// runX86Static runs a static x86-64 image under qemu, returning its exit code.
// A static image needs no loader and no libraries, so unlike runX86Dyn it wants
// only the emulator -- no sysroot.
func runX86Static(t *testing.T, image []byte) int {
	t.Helper()
	qemu := testenv.Tool(t, "qemu-x86_64")
	bin := filepath.Join(t.TempDir(), "prog")
	require.NoError(t, os.WriteFile(bin, image, 0o755))
	out, err := exec.Command(qemu, bin).CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		t.Fatalf("run: %v\n%s", err, out)
	}
	return 0
}
