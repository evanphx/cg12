package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// A pointer advance that sits immediately next to its load folds into a
// pre/post-indexed access. The forward loop keeps the loaded value separate
// from the pointer base, so the post-indexed load is architecturally valid;
// `*--p` decrements then loads (pre-index).
func TestWritebackFolding(t *testing.T) {
	src := `
long fwd(const long *p, long n){ long s=0; while(n--) s += *p++; return s; } /* post-index */
long bwd(const long *p, long n){ long s=0; while(n--) s += *--p; return s; } /* pre-index */`
	main := `extern long fwd(const long*, long);
extern long bwd(const long*, long);
int main(void){
    long b[4] = {1, 2, 3, 4};
    if (fwd(b, 4) != 10) return 1;
    long v[4] = {10, 20, 30, 40};
    if (bwd(v+4, 4) != 100) return 2;                     /* 40+30+20+10 */
    return 0;
}`
	m, err := cc.Compile("wb.c", src)
	require.NoError(t, err)
	opt.Run(m, opt.DefaultPipeline())
	_, code := buildAndRun(t, m, main)
	require.Equal(t, 0, code)

	m2, err := cc.Compile("wb.c", src)
	require.NoError(t, err)
	opt.Run(m2, opt.DefaultPipeline())
	o, err := arm64.CompileToObject(m2)
	require.NoError(t, err)
	text := arm64.Disassemble(o)
	require.Contains(t, text, "#-8]!", "bwd()'s *--p folds into a pre-indexed load")
}

func TestWritebackPostIndexFixedRegisters(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("postwb", ir.ClsL).Export()
	p := f.NewTemp("p", ir.ClsL)
	f.Temp(p).Fixed = true
	f.Temp(p).Reg = int(arm64.X10)
	next := f.NewTemp("next", ir.ClsL)
	f.Temp(next).Fixed = true
	f.Temp(next).Reg = int(arm64.X10)
	value := f.NewTemp("value", ir.ClsL)
	f.Temp(value).Fixed = true
	f.Temp(value).Reg = int(arm64.X11)

	e := f.Entry()
	e.Instrs = append(e.Instrs,
		ir.Instr{Op: ir.OLoadl, Cls: ir.ClsL, To: value, Args: []ir.Ref{p}},
		ir.Instr{Op: ir.OAdd, Cls: ir.ClsL, To: next, Args: []ir.Ref{p, f.Long(8)}},
	)
	e.Ret(value)

	o, err := arm64.CompileToObject(m)
	require.NoError(t, err)
	text := arm64.Disassemble(o)
	require.Contains(t, text, "ldr x11, [x10], #8")
}
