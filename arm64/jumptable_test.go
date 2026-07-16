package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// TestJumpTableArm64 exercises a dense switch that lowers to an indexed jump
// table (contiguous and negative-based ranges) end-to-end on arm64. It runs the
// same program through both emitters -- the assembly-text path (buildAndRun) and
// the machine-code object path (buildObjAndRun) -- so the shared jumpTable
// builder is validated in both backends by execution.
func TestJumpTableArm64(t *testing.T) {
	src := `
int contig(int n){ switch(n){
 case 0:return 100; case 1:return 101; case 2:return 102; case 3:return 103;
 case 4:return 104; case 5:return 105; case 6:return 106; case 7:return 107;
 case 8:return 108; case 9:return 109; default:return -1; } }
int negbase(int n){ switch(n){
 case -4:return 1; case -3:return 2; case -2:return 3; case -1:return 4;
 case 0:return 5; case 1:return 6; case 2:return 7; case 3:return 8; default:return 0; } }
int check(void){
	if(contig(0)!=100) return 1;
	if(contig(9)!=109) return 2;
	if(contig(5)!=105) return 3;
	if(contig(-1)!=-1) return 4;
	if(contig(10)!=-1) return 5;
	if(negbase(-4)!=1) return 6;
	if(negbase(0)!=5) return 7;
	if(negbase(3)!=8) return 8;
	if(negbase(-5)!=0) return 9;
	if(negbase(4)!=0) return 10;
	return 0;
}`
	main := `extern int check(void); int main(void){ return check(); }`

	// Compile a fresh module per subtest: lowering mutates the module in place.
	t.Run("text", func(t *testing.T) {
		m, err := cc.Compile("switch.c", src)
		require.NoError(t, err)
		_, code := buildAndRun(t, m, main)
		require.Equal(t, 0, code)
	})
	t.Run("object", func(t *testing.T) {
		m, err := cc.Compile("switch.c", src)
		require.NoError(t, err)
		_, code := buildObjAndRun(t, m, main)
		require.Equal(t, 0, code)
	})
}
