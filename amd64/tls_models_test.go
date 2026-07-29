package amd64_test

import (
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/internal/backendtest"
	"github.com/evanphx/cg12/internal/testenv"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/require"
)

// hosted links the backend's object against libc with a C driver, instead of the
// freestanding _start the rest of this package's execution tests use.
//
// A thread-local test needs everything a freestanding image deliberately does not
// have. The generated code reads the thread pointer out of %fs:0, and in a
// -nostdlib image %fs has no base, so the very first instruction of every model
// faults on a null read; the initial-exec GOT slot is filled by the dynamic
// loader; and per-thread isolation is only observable with something that creates
// threads. All three arrive together with the C runtime, which is why these tests
// link against it and run a C main that calls the generated function -- the same
// arrangement arm64/mc_tls_test.go uses.
//
// It is also why the old comment in mc_tls_test.go said a thread-local could only
// be checked at the object level: the harness available then was the freestanding
// one. That is what changed, not the backend.
var hosted = backendtest.Harness{
	Machine:     backendtest.X8664,
	Compiler:    []string{"clang", "cc"},
	CompileArgs: []string{"--target=x86_64-linux-gnu"},
	Linker:      []string{"clang", "cc"},
	LinkArgs:    []string{"--target=x86_64-linux-gnu"},
}

// requireHostedTLS skips when this host cannot run a libc-linked x86-64 image.
//
// The freestanding harness is happy under an emulator, because a static image with
// no libc asks nothing of its environment. These images ask for a dynamic loader
// and a C library built for the target, which an emulator alone does not supply --
// so unlike a missing tool, this is not something CG12_REQUIRE_TOOLS should turn
// into a failure. On an x86-64 host nothing is skipped.
func requireHostedTLS(t *testing.T) {
	t.Helper()
	if !backendtest.X8664.Native() {
		t.Skip("a libc-linked x86-64 image needs an x86-64 host: an emulator would also need a target sysroot")
	}
}

// bumpModule builds bump() = ++tls_counter, reading and writing a thread-local
// through whichever TLS model the options select. Two accesses, so both the load
// and the store address the variable.
func bumpModule() *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc("bump", ir.ClsW).Export()
	e := f.Entry()
	addr := f.ThreadSym("tls_counter")
	v := e.Add(ir.ClsW, e.Load(ir.ClsW, addr), f.Word(1))
	e.Store(v, addr)
	e.Ret(v)
	return m
}

// tlsFieldModule builds read() = *(int *)((char *)&tls_pair + off), a thread-local
// reference carrying an offset into the variable.
//
// The fluent builder has no ThreadSym-with-offset (ir.Func.ThreadSym takes only a
// name), but the offset field is not hypothetical: the address-folding pass and
// rematerialization both produce symbol constants with one, and the two models
// place it differently -- local-exec folds it into the relocation's addend, while
// initial-exec cannot, because the GOT slot it reads is per-symbol. So the constant
// is built here directly.
func tlsFieldModule(off int64) *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc("read", ir.ClsW).Export()
	f.Consts = append(f.Consts, ir.Const{
		Kind: ir.ConstSym, Cls: ir.ClsP, Sym: "tls_pair", Int: off, Thread: true,
	})
	addr := ir.Ref{Kind: ir.RefConst, ID: uint32(len(f.Consts) - 1)}
	e := f.Entry()
	e.Ret(e.Load(ir.ClsW, addr))
	return m
}

// compileTLS compiles m for one TLS model and returns the parsed object.
func compileTLS(t *testing.T, m *ir.Module, model amd64.TLSModel) *obj.Object {
	t.Helper()
	code, err := amd64.CompileObjectWith(m, amd64.Options{TLSModel: model})
	require.NoError(t, err)
	o, err := obj.ReadELF(code)
	require.NoError(t, err)
	return o
}

// tlsRelocs are m's relocations against sym, in emission order.
func tlsRelocs(o *obj.Object, sym string) []obj.Reloc {
	var out []obj.Reloc
	for _, r := range o.Relocs {
		if r.Sym == sym {
			out = append(out, r)
		}
	}
	return out
}

// requireTLSTyped asserts that sym is typed STT_TLS, which is what tells a linker
// to resolve it against a module's TLS block rather than as an address. Every model
// depends on it, and obj/ derives it from the relocation type -- so a model whose
// relocation obj/ does not recognize as thread-local silently loses it.
func requireTLSTyped(t *testing.T, o *obj.Object, sym string) {
	t.Helper()
	for _, s := range o.Syms {
		if s.Name == sym {
			require.Truef(t, s.TLS, "%s must be an STT_TLS symbol", sym)
			return
		}
	}
	t.Fatalf("no symbol named %s in the object", sym)
}

// --- local-exec: the model every existing caller already gets ---------------

// localExecBump is the exact text local-exec emitted for bumpModule before there
// was a model to choose, captured from the compiler at the commit that introduced
// TLSModel.
//
// Pinning the bytes rather than the shape is the point. Local-exec is what every
// amd64 caller gets today, and Options.TLSModel defaults to it, so the one
// regression this work could cause is a changed local-exec sequence -- which would
// not fail any test that only checked for "a TPOFF32 relocation somewhere". The
// two accesses show as two `mov %fs:0` + `add $imm32` pairs at 0x04 and 0x1a, with
// the relocated immediates at 0x10 and 0x26.
//
// The `+ 1` between them was `mov $1,%r11d; add %r11d,%esi` when this constant was
// first captured, and is now the one-instruction `add $1,%esi`: immediate folding
// (imm.go) reached it. Both TLS sequences themselves are byte-identical to the
// capture, which is what this constant is here to protect; only the arithmetic
// around them got shorter, which moved the second relocation from 0x2c to 0x26.
const localExecBump = "554889e5" + // push %rbp; mov %rsp,%rbp
	"644c8b1c2500000000" + "4981c300000000" + // mov %fs:0,%r11; add $tpoff,%r11
	"418b33" + "83c601" + // mov (%r11),%esi; add $1,%esi
	"644c8b1c2500000000" + "4981c300000000" + // mov %fs:0,%r11; add $tpoff,%r11
	"418933" + "89f0" + // mov %esi,(%r11); mov %esi,%eax
	"4889ec5dc3" // mov %rbp,%rsp; pop %rbp; ret

func TestTLSLocalExecUnchanged(t *testing.T) {
	// The default options and an explicit local-exec must be the same thing, or
	// the default changed behaviour for callers who never asked for a model.
	byDefault, err := amd64.CompileObject(bumpModule())
	require.NoError(t, err)
	explicit, err := amd64.CompileObjectWith(bumpModule(), amd64.Options{TLSModel: amd64.TLSLocalExec})
	require.NoError(t, err)
	require.Equal(t, byDefault, explicit, "Options{} must mean local-exec, byte for byte")

	o, err := obj.ReadELF(byDefault)
	require.NoError(t, err)
	require.Equal(t, localExecBump, hex.EncodeToString(o.Text), "local-exec text changed")

	relocs := tlsRelocs(o, "tls_counter")
	require.Len(t, relocs, 2, "one relocation per access")
	for _, r := range relocs {
		require.Equal(t, uint32(obj.R_X86_64_TPOFF32), r.Type)
		require.Zero(t, r.Addend, "local-exec patches an immediate in place, so no PC correction")
	}
	require.Equal(t, uint64(0x10), relocs[0].Offset)
	require.Equal(t, uint64(0x26), relocs[1].Offset)
	requireTLSTyped(t, o, "tls_counter")
}

// An offset into the variable folds into the local-exec addend: the whole distance
// from the thread pointer is one link-time constant, so it stays one instruction.
func TestTLSLocalExecFoldsSymbolOffset(t *testing.T) {
	o := compileTLS(t, tlsFieldModule(8), amd64.TLSLocalExec)
	relocs := tlsRelocs(o, "tls_pair")
	require.Len(t, relocs, 1)
	require.Equal(t, uint32(obj.R_X86_64_TPOFF32), relocs[0].Type)
	require.EqualValues(t, 8, relocs[0].Addend)
}

// --- initial-exec ----------------------------------------------------------

// Initial-exec is `mov %fs:0, rd` then `add rd, sym@gottpoff(%rip)`: the offset is
// no longer an immediate but a GOT slot the loader fills, so the same two
// instructions reach a variable that lives in a shared library.
func TestTLSInitialExecSequence(t *testing.T) {
	o := compileTLS(t, bumpModule(), amd64.TLSInitialExec)
	text := hex.EncodeToString(o.Text)

	// Same register and same first instruction as local-exec; only the add differs,
	// from an imm32 form (49 81 c3) to a RIP-relative memory form (4c 03 1d).
	require.Contains(t, text, "644c8b1c2500000000"+"4c031d00000000",
		"expected mov %%fs:0,%%r11 followed by add sym@gottpoff(%%rip),%%r11 in\n"+text)

	relocs := tlsRelocs(o, "tls_counter")
	require.Len(t, relocs, 2, "one relocation per access")
	for _, r := range relocs {
		require.Equal(t, uint32(obj.R_X86_64_GOTTPOFF), r.Type)
		// -4 and not 0: the fixup is the displacement field, and RIP is measured
		// from the end of the instruction, four bytes further on.
		require.EqualValues(t, -4, r.Addend, "GOTTPOFF must correct for the displacement it sits in")
	}
	requireTLSTyped(t, o, "tls_counter")

	// Initial-exec costs nothing in code size here, which is worth pinning because
	// it is easy to assume otherwise: `add $imm32, %r11` and
	// `add disp32(%rip), %r11` are both 7 bytes (REX, opcode, ModRM, then four
	// bytes the relocation patches), so the two models emit texts of equal length
	// and differ only in the ModRM byte.
	require.Len(t, o.Text, len(localExecBump)/2, "initial-exec must be the same length as local-exec")
}

// The GOT slot holds the variable's own thread-pointer offset and nothing else, so
// an offset into the variable cannot ride along in the addend -- it becomes a third
// instruction. Getting this wrong would put the offset in the addend, where the
// linker would apply it to the slot's address and read a neighbouring slot.
func TestTLSInitialExecSymbolOffsetIsASeparateAdd(t *testing.T) {
	o := compileTLS(t, tlsFieldModule(8), amd64.TLSInitialExec)
	text := hex.EncodeToString(o.Text)

	relocs := tlsRelocs(o, "tls_pair")
	require.Len(t, relocs, 1)
	require.Equal(t, uint32(obj.R_X86_64_GOTTPOFF), relocs[0].Type)
	require.EqualValues(t, -4, relocs[0].Addend, "the offset must not be folded into the addend")

	// mov %fs:0,%r11; add tls_pair@gottpoff(%rip),%r11; add $8,%r11
	require.Contains(t, text, "644c8b1c2500000000"+"4c031d00000000"+"4981c308000000",
		"expected the offset as its own add in\n"+text)
}

// --- general-dynamic -------------------------------------------------------

// General-dynamic is not implemented, and asking for it must fail rather than fall
// back to a model that would read the wrong thread's memory. The error names the
// obj/ relocations that are missing, because that is the work that unblocks it.
func TestTLSGeneralDynamicIsARefusal(t *testing.T) {
	_, err := amd64.CompileObjectWith(bumpModule(), amd64.Options{TLSModel: amd64.TLSGeneralDynamic})
	require.Error(t, err)
	require.Contains(t, err.Error(), "general-dynamic")
	require.Contains(t, err.Error(), "R_X86_64_TLSGD")

	// A module with no thread-local in it has nothing to refuse.
	m := ir.NewModule()
	f := m.NewFunc("answer", ir.ClsW).Export()
	f.Entry().Ret(f.Word(42))
	_, err = amd64.CompileObjectWith(m, amd64.Options{TLSModel: amd64.TLSGeneralDynamic})
	require.NoError(t, err)
}

func TestTLSModelNames(t *testing.T) {
	require.Equal(t, "local-exec", amd64.TLSLocalExec.String())
	require.Equal(t, "initial-exec", amd64.TLSInitialExec.String())
	require.Equal(t, "general-dynamic", amd64.TLSGeneralDynamic.String())
}

// --- executing the generated code ------------------------------------------

// bumpDriver is a C main that hands the generated bump() a thread-local it owns and
// checks both the value it returned and the variable it left behind.
const bumpDriver = `
_Thread_local int tls_counter = 41;
extern int bump(void);
int main(void) { return (bump() == 42 && tls_counter == 42) ? 0 : 1; }
`

// threadDriver runs the generated bump() on two threads at once. Each sets its own
// starting value and bumps three times, so the answers can only both be right if
// the generated code reached each thread's own instance.
const threadDriver = `
#include <pthread.h>
_Thread_local int tls_counter = 0;
extern int bump(void);
static void *worker(void *arg) {
  int *slot = arg;
  tls_counter = *slot;
  int r = 0;
  for (int i = 0; i < 3; i++) r = bump();
  *slot = r;
  return 0;
}
int main(void) {
  int a = 100, b = 200;
  pthread_t ta, tb;
  pthread_create(&ta, 0, worker, &a);
  pthread_create(&tb, 0, worker, &b);
  pthread_join(ta, 0);
  pthread_join(tb, 0);
  return (a == 103 && b == 203) ? 0 : 1;
}
`

// runBump compiles bumpModule for a model, links it against a C driver, and runs it.
func runBump(t *testing.T, model amd64.TLSModel, driver string, linkArgs ...string) int {
	t.Helper()
	requireHostedTLS(t)
	code, err := amd64.CompileObjectWith(bumpModule(), amd64.Options{TLSModel: model})
	require.NoError(t, err)
	h := hosted.WithLinkArgs(linkArgs...)
	return h.Exit(t, backendtest.Object(code), backendtest.CSource(driver))
}

// Local-exec really reads and writes the C runtime's thread-local. This is the
// check mc_tls_test.go could not make, and what it adds over the relocation check
// is the thread pointer: an object can carry a correct-looking TPOFF32 and still
// compute the address from the wrong base.
func TestTLSLocalExecRunsNatively(t *testing.T) {
	require.Equal(t, 0, runBump(t, amd64.TLSLocalExec, bumpDriver))
}

func TestTLSInitialExecRunsNatively(t *testing.T) {
	require.Equal(t, 0, runBump(t, amd64.TLSInitialExec, bumpDriver))
}

// Two threads see two instances. A model that computed a fixed address -- reaching
// the variable through the image rather than through the thread pointer -- passes
// the single-threaded test above and fails this one.
func TestTLSLocalExecIsPerThread(t *testing.T) {
	require.Equal(t, 0, runBump(t, amd64.TLSLocalExec, threadDriver, "-pthread"))
}

func TestTLSInitialExecIsPerThread(t *testing.T) {
	require.Equal(t, 0, runBump(t, amd64.TLSInitialExec, threadDriver, "-pthread"))
}

// Initial-exec exists for the thread-local that is not in the executable, and this
// is the case that shows it: the generated bump() and the variable both go into a
// shared library, so the offset genuinely comes from a GOT slot the loader fills at
// load time. Nothing is relaxed away and nothing is a link-time constant.
//
// It is also where obj/'s STT_TLS typing of a GOTTPOFF symbol is put in front of a
// real linker. Typing the symbol as ordinary data instead is a link error and not
// a wrong answer, so nothing but a linker will report it -- and it is the failure
// mode general-dynamic would hit today, because obj/ does not recognize a TLSGD
// relocation as thread-local at all.
//
// Local-exec cannot do this at all, and the second half of the test says so: a
// TP-relative immediate is only a constant in the main executable, so linking the
// same module into a shared object has to be refused.
//
// It drives the compiler driver directly rather than through the harness, which
// builds one image from one link command; this needs two links and wants one of
// them to fail.
func TestTLSInitialExecInSharedLibrary(t *testing.T) {
	requireHostedTLS(t)
	cc := testenv.Tool(t, hosted.Compiler...)

	ie, err := amd64.CompileObjectWith(bumpModule(), amd64.Options{TLSModel: amd64.TLSInitialExec})
	require.NoError(t, err)
	le, err := amd64.CompileObjectWith(bumpModule(), amd64.Options{TLSModel: amd64.TLSLocalExec})
	require.NoError(t, err)

	dir := t.TempDir()
	libSrc := filepath.Join(dir, "lib.c")
	require.NoError(t, os.WriteFile(libSrc, []byte(`
_Thread_local int tls_counter = 41;
extern int bump(void);
int probe(void) { return (bump() == 42 && tls_counter == 42) ? 0 : 1; }
`), 0o644))
	mainSrc := filepath.Join(dir, "main.c")
	require.NoError(t, os.WriteFile(mainSrc, []byte(`
extern int probe(void);
int main(void) { return probe(); }
`), 0o644))

	buildLib := func(name string, object []byte) ([]byte, error) {
		objPath := filepath.Join(dir, name+".o")
		require.NoError(t, os.WriteFile(objPath, object, 0o644))
		lib := filepath.Join(dir, "lib"+name+".so")
		cmd := exec.Command(cc, "-shared", "-fPIC", "-o", lib, objPath, libSrc)
		return cmd.CombinedOutput()
	}

	out, err := buildLib("ie", ie)
	require.NoErrorf(t, err, "linking initial-exec into a shared library failed:\n%s", out)

	out, err = buildLib("le", le)
	require.Errorf(t, err, "a local-exec thread-local must not link into a shared library, but it did:\n%s", out)

	bin := filepath.Join(dir, "prog")
	out, err = exec.Command(cc, "-o", bin, mainSrc, "-L", dir, "-lie", "-Wl,-rpath,"+dir).CombinedOutput()
	require.NoErrorf(t, err, "linking against the initial-exec library failed:\n%s", out)

	require.NoError(t, exec.Command(bin).Run(), "the shared library's thread-local access failed")
}
