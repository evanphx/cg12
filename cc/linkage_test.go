package cc_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// linkWithC compiles cg12Src with cg12 and peerSrc with gcc, links them together,
// runs the result, and returns its exit code. This is the shape no test had: every
// existing one externs a FUNCTION from its driver, and functions work because a
// call is a relocation. Sharing a VARIABLE is what was broken in both directions.
func linkWithC(t *testing.T, cg12Src, peerSrc string) int {
	t.Helper()
	gcc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("gcc not available")
	}
	m, err := cc.Compile("unit.c", cg12Src)
	require.NoError(t, err)
	code, err := arm64.CompileObject(m)
	require.NoError(t, err)

	dir := t.TempDir()
	obj := filepath.Join(dir, "unit.o")
	peer := filepath.Join(dir, "peer.c")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(obj, code, 0o644))
	require.NoError(t, os.WriteFile(peer, []byte(peerSrc), 0o644))

	out, err := exec.Command(gcc, "-w", "-o", bin, obj, peer).CombinedOutput()
	require.NoErrorf(t, err, "link: %s", out)

	if err := exec.Command(bin).Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		t.Fatalf("run: %v", err)
	}
	return 0
}

// `extern int x;` promises x lives in another translation unit. Defining our own
// would give this unit a private zero-filled x and quietly read that instead.
func TestExternVariableReadsTheRealDefinition(t *testing.T) {
	require.Equal(t, 0, linkWithC(t,
		`extern int shared; extern long wide; int use(void){ return shared + (int)wide; }`,
		`int shared = 40; long wide = 2;
		 extern int use(void);
		 int main(void){ return use() == 42 ? 0 : 1; }`))
}

// The mirror: a file-scope variable has external linkage and must be visible to
// the linker, or nothing else can reference it.
func TestGlobalIsVisibleToTheLinker(t *testing.T) {
	require.Equal(t, 0, linkWithC(t,
		`int shared = 42; int tentative;`,
		`extern int shared, tentative;
		 int main(void){ return (shared == 42 && tentative == 0) ? 0 : 1; }`))
}

// static is internal linkage: it must NOT collide with another unit's symbol of
// the same name, and each must see its own.
func TestStaticGlobalStaysPrivate(t *testing.T) {
	require.Equal(t, 0, linkWithC(t,
		`static int counter = 7; int ours(void){ return counter; }`,
		`static int counter = 9;                 /* a different variable entirely */
		 extern int ours(void);
		 int main(void){ return (ours() == 7 && counter == 9) ? 0 : 1; }`))
}

// A thread-local is one variable per thread. Sharing one across threads is the
// silent failure: it compiles, runs, and gives the wrong answer under threads.
func TestThreadLocalIsPerThread(t *testing.T) {
	require.Equal(t, 0, linkWithC(t,
		`_Thread_local int tl = 41;
		 int bump(void){ return ++tl; }
		 int peek(void){ return tl; }`,
		`#include <pthread.h>
		 extern int bump(void), peek(void);
		 static void *worker(void *arg){
		   *(int*)arg = bump();     /* the child's own tl, from 41 */
		   return 0;
		 }
		 int main(void){
		   if(bump() != 42) return 1;        /* the main thread's tl */
		   int child = 0; pthread_t th;
		   if(pthread_create(&th, 0, worker, &child)) return 2;
		   pthread_join(th, 0);
		   if(child != 42) return 3;         /* the child started from its own 41 */
		   if(peek() != 42) return 4;        /* and did not touch ours */
		   return 0;
		 }`))
}

// A thread-local defined elsewhere is reached through the TLS ABI too, not as an
// ordinary global.
func TestExternThreadLocal(t *testing.T) {
	require.Equal(t, 0, linkWithC(t,
		`extern _Thread_local int etl; int read_etl(void){ return etl; }`,
		`_Thread_local int etl = 42;
		 extern int read_etl(void);
		 int main(void){ return read_etl() == 42 ? 0 : 1; }`))
}
