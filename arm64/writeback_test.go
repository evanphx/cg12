package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// A pointer advance that sits immediately next to its load folds into a
// pre/post-indexed access. `*q++` in the loop condition keeps the load and the
// increment adjacent (post-index); `*--p` decrements then loads (pre-index).
func TestWritebackFolding(t *testing.T) {
	src := `
long slen(const unsigned char *p){ const unsigned char *q=p; while(*q++); return q-p; } /* post-index */
long bwd(const long *p, long n){ long s=0; while(n--) s += *--p; return s; } /* pre-index */`
	main := `extern long slen(const unsigned char*);
extern long bwd(const long*, long);
int main(void){
    if (slen((const unsigned char*)"abc") != 4) return 1; /* 3 chars + NUL */
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
	require.Contains(t, text, "], #1", "slen()'s *q++ folds into a post-indexed load")
	require.Contains(t, text, "#-8]!", "bwd()'s *--p folds into a pre-indexed load")
}
