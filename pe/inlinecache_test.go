package pe_test

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/pe"
	"github.com/stretchr/testify/require"
)

// An interpreter with a monomorphic inline cache: per-position data attached to a
// SEND (method-dispatch) instruction. ic[pc] caches, for the call site at pc, the
// last receiver class it saw and the method that resolved to. On a hit it skips the
// slow lookup. The cache array is runtime state (allocated by calloc, mutated as it
// warms), but it is indexed by pc -- which is green -- so each call site's slot is
// at a statically-known offset. Specializing resolves that indexing: every SEND in
// the program gets its own cache slot, and the check/hit/miss/update logic
// residualizes around it.
const toyICInterp = `
void __cg12_merge_point(int pc, int sp);
void *calloc(unsigned long n, unsigned long sz);
long rt_lookup(long klass, long sel);    // slow method resolution (the miss path)
long rt_invoke(long method, long recv);  // call the resolved method

enum { OP_HALT, OP_PUSH, OP_INPUT, OP_LOAD, OP_STORE, OP_JMP, OP_JZ, OP_SUB, OP_SEND };

struct IC { long klass; long method; };

long interp(const unsigned char *code, const long *in) {
    long stack[64];
    long var[4];
    int sp = 0;
    int pc = 0;
    struct IC *ic = calloc(256, sizeof(struct IC));  // one cache slot per position
    for (;;) {
        __cg12_merge_point(pc, sp);
        switch (code[pc]) {
        case OP_PUSH:  stack[sp++] = code[pc+1];       pc += 2; break;
        case OP_INPUT: stack[sp++] = in[code[pc+1]];   pc += 2; break;
        case OP_LOAD:  stack[sp++] = var[code[pc+1]];  pc += 2; break;
        case OP_STORE: var[code[pc+1]] = stack[--sp];  pc += 2; break;
        case OP_JMP:   pc = code[pc+1];                        break;
        case OP_JZ:    if (stack[--sp] == 0) pc = code[pc+1]; else pc += 2; break;
        case OP_SUB:   sp--; stack[sp-1] -= stack[sp];  pc += 1; break;
        case OP_SEND: {
            long recv = stack[--sp];
            long klass = recv & 0xff;
            long method;
            if (ic[pc].klass == klass) {            // per-position cache check
                method = ic[pc].method;             // hit
            } else {
                method = rt_lookup(klass, code[pc+1]);  // miss: slow lookup
                ic[pc].klass = klass;
                ic[pc].method = method;             // fill the cache slot
            }
            stack[sp++] = rt_invoke(method, recv);
            pc += 2; break;
        }
        case OP_HALT:  return stack[sp-1];
        }
    }
}
`

const (
	icHALT = iota
	icPUSH
	icINPUT
	icLOAD
	icSTORE
	icJMP
	icJZ
	icSUB
	icSEND
)

const icSEL = 7 // the selector the SEND dispatches on

func icLookup(klass, sel int64) int64   { return klass*1000 + sel }
func icInvoke(method, recv int64) int64 { return method + recv }

// refRunIC computes the program's result directly -- the cache is transparent, so
// the reference need not model it.
func refRunIC(recv, loops int64) int64 {
	result := int64(0)
	for i := loops; i != 0; i-- {
		result = icInvoke(icLookup(recv&0xff, icSEL), recv)
	}
	return result
}

func TestSpecializeInlineCache(t *testing.T) {
	loop, end := byte(8), byte(27)
	prog := []byte{
		icINPUT, 0, icSTORE, 0, // recv = in[0]
		icINPUT, 1, icSTORE, 1, // i = in[1]  (loop count)
		icLOAD, 1, icJZ, end, // loop: if (i == 0) goto end
		icLOAD, 0, icSEND, icSEL, icSTORE, 2, // result = recv.send(SEL)
		icLOAD, 1, icPUSH, 1, icSUB, icSTORE, 1, // i -= 1
		icJMP, loop,
		icLOAD, 2, icHALT, // end: return result
	}

	m, err := cc.Compile("toy.c", toyICInterp)
	require.NoError(t, err)
	spec, name, inputs, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	residual := spec.String()
	t.Logf("residual:\n%s", residual)
	require.Len(t, inputs, 2)
	// The dispatch is gone, but the inline-cache machinery is right there: the
	// runtime lookup on the miss path, and stores that fill the cache slot.
	require.NotContains(t, residual, "switch")
	require.Contains(t, residual, "call $rt_lookup")
	require.Contains(t, residual, "call $rt_invoke")

	// The cache hit and miss paths re-converge: the send is emitted once, not
	// duplicated down each arm.
	require.Equal(t, 1, strings.Count(spec.String(), "call $rt_invoke"),
		"the shared tail after the cache check is not duplicated")

	opt.Run(spec, opt.DefaultPipeline())

	lookups := 0
	mc, err := interp.New(spec,
		interp.WithExtern("rt_lookup", func(_ *interp.Machine, a []interp.Value) (interp.Value, error) {
			lookups++
			return interp.L(icLookup(a[0].I64(), a[1].I64())), nil
		}),
		interp.WithExtern("rt_invoke", func(_ *interp.Machine, a []interp.Value) (interp.Value, error) {
			return interp.L(icInvoke(a[0].I64(), a[1].I64())), nil
		}))
	require.NoError(t, err)

	// recv = 5 (class 5), loop 10 times.
	v, err := mc.Call(name, interp.L(5), interp.L(10))
	require.NoError(t, err)
	require.Equal(t, refRunIC(5, 10), v.I64())
	// The cache did its job: the slow lookup ran once across ten dispatches.
	require.Equal(t, 1, lookups, "the inline cache hit after warming up")
}
