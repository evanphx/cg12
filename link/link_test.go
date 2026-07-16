package link_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/link"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolchain finds a C compiler producing native AArch64 objects.
func toolchain() (string, bool) {
	if p, err := exec.LookPath("aarch64-linux-gnu-gcc"); err == nil {
		return p, true
	}
	if runtime.GOARCH == "arm64" {
		for _, c := range []string{"cc", "gcc", "clang"} {
			if p, err := exec.LookPath(c); err == nil {
				return p, true
			}
		}
	}
	return "", false
}

// linkRun links a merged object with a C driver, runs it, and returns the code.
func linkRun(t *testing.T, data []byte, cmain string) int {
	t.Helper()
	cc, ok := toolchain()
	if !ok {
		t.Skip("no AArch64 toolchain available")
	}
	dir := t.TempDir()
	objPath := filepath.Join(dir, "linked.o")
	cPath := filepath.Join(dir, "main.c")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(objPath, data, 0o644))
	require.NoError(t, os.WriteFile(cPath, []byte(cmain), 0o644))
	out, err := exec.Command(cc, "-o", bin, objPath, cPath).CombinedOutput()
	require.NoErrorf(t, err, "link failed: %s", out)
	if err := exec.Command(bin).Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		t.Fatalf("run: %v", err)
	}
	return 0
}

// moduleHelper builds helper() = 41.
func moduleHelper() *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc("helper", ir.ClsW).Export()
	f.Entry().Ret(f.Word(41))
	return m
}

// moduleCaller builds caller() = helper() + 1, referencing an external helper.
func moduleCaller() *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc("caller", ir.ClsW).Export()
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, e.Call(ir.ClsW, f.Sym("helper", 0)), f.Word(1)))
	return m
}

// The linker resolves a call between two IR modules — the CALL26 is patched and
// the relocation dropped — and the merged object links and runs.
func TestLinkResolvesCrossModuleCallFromIR(t *testing.T) {
	l := link.New()
	require.NoError(t, l.AddModule(moduleHelper()))
	require.NoError(t, l.AddModule(moduleCaller()))

	linked, err := l.Link()
	require.NoError(t, err)

	// helper is defined and its call resolved: no relocation names it.
	for _, r := range linked.Relocs {
		assert.NotEqual(t, "helper", r.Sym, "the cross-module call was resolved by the linker")
	}
	// Both functions are present in the merged object.
	names := map[string]bool{}
	for _, s := range linked.Syms {
		names[s.Name] = true
	}
	assert.True(t, names["helper"] && names["caller"])

	data, err := linked.MarshalELF()
	require.NoError(t, err)
	code := linkRun(t, data, `extern int caller(void); int main(void){ return caller()==42 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// The linker resolves the same cross-module call for amd64 output when driven by
// the amd64 backend: the x86-64 PLT32 relocation is patched and dropped, proving
// the common Backend interface and the machine-generic branch patcher.
func TestLinkResolvesCrossModuleCallAMD64(t *testing.T) {
	l := link.NewWith(amd64.Backend{})
	require.NoError(t, l.AddModule(moduleHelper()))
	require.NoError(t, l.AddModule(moduleCaller()))

	linked, err := l.Link()
	require.NoError(t, err)

	assert.Equal(t, uint16(obj.EM_X86_64), linked.Machine)
	for _, r := range linked.Relocs {
		assert.NotEqual(t, "helper", r.Sym, "the cross-module call was resolved by the linker")
	}
	names := map[string]bool{}
	for _, s := range linked.Syms {
		names[s.Name] = true
	}
	assert.True(t, names["helper"] && names["caller"])
}

// The same link works when the inputs are architecture .o files rather than IR.
func TestLinkFromObjectFiles(t *testing.T) {
	ha, err := arm64.CompileObject(moduleHelper())
	require.NoError(t, err)
	ca, err := arm64.CompileObject(moduleCaller())
	require.NoError(t, err)

	l := link.New()
	require.NoError(t, l.AddObjectFile(ha))
	require.NoError(t, l.AddObjectFile(ca))
	data, err := l.LinkToELF()
	require.NoError(t, err)

	code := linkRun(t, data, `extern int caller(void); int main(void){ return caller()==42 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// Mixed input: one IR module and one prebuilt .o.
func TestLinkMixedInputs(t *testing.T) {
	helperObj, err := arm64.CompileObject(moduleHelper())
	require.NoError(t, err)

	l := link.New()
	require.NoError(t, l.AddObjectFile(helperObj))  // from a .o
	require.NoError(t, l.AddModule(moduleCaller())) // from IR
	data, err := l.LinkToELF()
	require.NoError(t, err)

	code := linkRun(t, data, `extern int caller(void); int main(void){ return caller()==42 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// Defining the same global symbol twice is a link error.
func TestLinkDuplicateSymbol(t *testing.T) {
	l := link.New()
	require.NoError(t, l.AddModule(moduleHelper()))
	require.NoError(t, l.AddModule(moduleHelper()))
	_, err := l.Link()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate symbol")
}
