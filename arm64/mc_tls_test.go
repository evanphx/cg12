package arm64_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// runObjectCC links a pre-compiled object with a C driver and extra compiler
// flags, runs it, and returns the exit code.
func runObjectCC(t *testing.T, data []byte, cmain string, extra ...string) int {
	t.Helper()
	cc := assembler(t)
	dir := t.TempDir()
	objPath := filepath.Join(dir, "u.o")
	cPath := filepath.Join(dir, "main.c")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(objPath, data, 0o644))
	require.NoError(t, os.WriteFile(cPath, []byte(cmain), 0o644))
	args := append([]string{"-o", bin, objPath, cPath}, extra...)
	out, err := exec.Command(cc, args...).CombinedOutput()
	require.NoErrorf(t, err, "link failed: %s", out)
	if err := exec.Command(bin).Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		t.Fatalf("run: %v", err)
	}
	return 0
}

// bumpModule builds bump() = ++tls_counter, addressing a thread-local variable.
func bumpModule() *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc("bump", ir.ClsW).Export()
	e := f.Entry()
	addr := f.ThreadSym("tls_counter") // &tls_counter, per-thread
	v := e.Add(ir.ClsW, e.Load(ir.ClsW, addr), f.Word(1))
	e.Store(v, addr)
	e.Ret(v)
	return m
}

// A thread-local variable is addressed through the TLS ABI: the generated bump()
// reads and writes the C runtime's _Thread_local counter.
func TestObjEmitThreadLocalAccess(t *testing.T) {
	data, err := arm64.CompileObject(bumpModule())
	require.NoError(t, err)
	code := runObject(t, data, `
_Thread_local int tls_counter = 41;
extern int bump(void);
int main(void){ return (bump() == 42 && tls_counter == 42) ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// Each thread sees its own instance: two threads bump independently.
func TestObjEmitThreadLocalPerThread(t *testing.T) {
	data, err := arm64.CompileObject(bumpModule())
	require.NoError(t, err)
	code := runObjectCC(t, data, `
#include <pthread.h>
_Thread_local int tls_counter = 0;
extern int bump(void);
static void *worker(void *arg){
  int *slot = arg;
  tls_counter = *slot;               // this thread's starting value
  int r = 0;
  for (int i = 0; i < 3; i++) r = bump();
  *slot = r;
  return 0;
}
int main(void){
  int a = 100, b = 200;
  pthread_t ta, tb;
  pthread_create(&ta, 0, worker, &a);
  pthread_create(&tb, 0, worker, &b);
  pthread_join(ta, 0);
  pthread_join(tb, 0);
  return (a == 103 && b == 203) ? 0 : 1; // isolated per thread
}`, "-pthread")
	require.Equal(t, 0, code)
}

// A GC poll can use a per-thread flag: the strategy addresses it via TLS.
func TestGCStrategyThreadLocalPoll(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("spin", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	done := f.NewBlock("done")
	start.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, n), done, body)
	body.Safepoint()
	i2 := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, i2)
	done.Ret(i)

	opts := arm64.Options{GC: arm64.PollStrategy{
		FlagSym: "gc_poll_flag", StubSym: "gc_safepoint", ThreadLocal: true,
	}}
	data, err := arm64.CompileObjectWith(m, opts)
	require.NoError(t, err)

	code := runObject(t, data, `
extern int spin(int);
_Thread_local unsigned char gc_poll_flag = 1; // per-thread collection request
int g_polls = 0;
void gc_safepoint(void){ g_polls++; gc_poll_flag = 0; }
int main(void){ spin(100); return g_polls == 1 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// The thread-pointer offset is split across two adds -- a high half and a low
// half -- so the low one must be the no-check relocation. Paired with the
// checking variant it happens to work for a small block and then fails to link
// the moment a thread-local sits more than 4 KiB in, which is exactly the case
// this pads out. Our own linker does not check the fit, so only a real linker
// can show the difference.
func TestObjEmitThreadLocalPastTheLowBits(t *testing.T) {
	data, err := arm64.CompileObject(lateBumpModule())
	require.NoError(t, err)
	code := runObject(t, data, `
_Thread_local char pad[8192] = {1};   // pushes late_counter past a 12-bit offset
_Thread_local int late_counter = 41;
extern int bump(void);
int main(void){ return (bump() == 42 && late_counter == 42 && pad[0] == 1) ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// lateBumpModule is bumpModule against a thread-local the driver places beyond
// the reach of a single 12-bit offset.
func lateBumpModule() *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc("bump", ir.ClsW).Export()
	e := f.Entry()
	addr := f.ThreadSym("late_counter")
	v := e.Add(ir.ClsW, e.Load(ir.ClsW, addr), f.Word(1))
	e.Store(v, addr)
	e.Ret(v)
	return m
}
