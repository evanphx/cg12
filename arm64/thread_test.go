package arm64_test

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// A computed-goto dispatch is threaded: each `goto *p` is its own indirect
// branch, so an interpreter's handlers each end in their own dispatch rather than
// funnelling through one shared branch. This checks a small bytecode interpreter
// both dispatches correctly and emits an indirect branch per goto site.
func TestComputedGotoIsThreaded(t *testing.T) {
	src := `
long interp(const unsigned char *code){
    static void * const tab[] = {&&H, &&ADD, &&PUSH};
    long acc = 0; const unsigned char *pc = code;
    goto *tab[*pc++];
 H:    return acc;
 ADD:  acc += (signed char)*pc++; goto *tab[*pc++];
 PUSH: acc = (signed char)*pc++;  goto *tab[*pc++];
}`
	main := `extern long interp(const unsigned char*);
int main(void){
    unsigned char code[] = {2, 10, 1, 5, 1, 7, 0}; /* PUSH 10; ADD 5; ADD 7; HALT */
    return interp(code) == 22 ? 0 : 1;
}`
	m, err := cc.Compile("i.c", src)
	require.NoError(t, err)
	opt.Run(m, opt.DefaultPipeline())
	_, code := buildAndRun(t, m, main)
	require.Equal(t, 0, code)

	m2, err := cc.Compile("i.c", src)
	require.NoError(t, err)
	opt.Run(m2, opt.DefaultPipeline())
	o, err := arm64.CompileToObject(m2)
	require.NoError(t, err)
	brs := strings.Count(arm64.Disassemble(o), "\tbr x")
	require.Greater(t, brs, 1, "threaded dispatch emits an indirect branch per goto site, not one funnel")
}

// TestThreadedInterpLoop runs a threaded bytecode interpreter whose dispatch
// loops -- so the computed-goto CFG is a mesh and its loop-carried state (acc,
// pc) is promoted to registers with a phi at every handler. It exercises the
// whole path: CoalescePhis unifying each variable across the mesh, DestructSSA
// placing the residual init copies before the entry's indirect branch, and the
// colourer keeping those copies off the branch-target register. Before that last
// piece was fixed, this shape miscompiled (a phi copy clobbered the jump target).
func TestThreadedInterpLoop(t *testing.T) {
	src := `
long interp(const unsigned char *code){
    static void * const tab[] = {&&H, &&IN, &&JN};
    long acc = 0; const unsigned char *pc = code;
    #define NEXT goto *tab[*pc++]
    NEXT;
 H:  return acc;
 IN: acc++; NEXT;
 JN: { long off = (signed char)*pc++; if (acc < 100) pc += off; } NEXT;
}`
	main := `extern long interp(const unsigned char*);
int main(void){
    unsigned char prog[] = {1, 2, (unsigned char)-3, 0}; /* INC; JN back-if<100; HALT */
    return interp(prog) == 100 ? 0 : 1;
}`
	m, err := cc.Compile("i.c", src)
	require.NoError(t, err)
	opt.Run(m, opt.DefaultPipeline())
	_, code := buildAndRun(t, m, main)
	require.Equal(t, 0, code)
}

// ThreadJumps (run during lowering) collapses empty forwarding blocks but must
// never bypass or drop a block whose address is taken by a computed goto -- an
// indirect branch reaches it with no explicit edge. This runs a computed-goto
// dispatch loop end to end to guard that invariant.
func TestThreadJumpsPreservesComputedGoto(t *testing.T) {
	src := `
long cg(int n){
    static void * const tab[] = {&&L0, &&L1, &&L2};
    long s = 0; int i = 0;
    goto *tab[0];
 L0: s += 1; i++; if (i >= n) return s; goto *tab[i%3];
 L1: s += 2; i++; if (i >= n) return s; goto *tab[i%3];
 L2: s += 3; i++; if (i >= n) return s; goto *tab[i%3];
}`
	main := `extern long cg(int);
int main(void){
    return (cg(6) == 12 && cg(1) == 1 && cg(3) == 6) ? 0 : 1; /* 1,2,3 cycling */
}`
	m, err := cc.Compile("cg.c", src)
	require.NoError(t, err)
	_, code := buildAndRun(t, m, main)
	require.Equal(t, 0, code)
}
