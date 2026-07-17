package cc_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// A construct cg12 cannot fold must be a diagnostic, not a default. Taking 0 for
// an unfoldable case label, or emitting nothing for a goto, produces a program
// that compiles, runs, and does something the source never asked for -- which is
// worse than not compiling.
func TestUnfoldableConstructsAreDiagnosed(t *testing.T) {
	for _, c := range []struct{ name, src, want string }{
		{
			// collectLabels pre-allocates a block for every label reachable from a
			// goto, so a name still missing here is one that does not exist -- and
			// emitting nothing for a jump is not a failed jump, it is a fall-through
			// to whatever follows. The parser catches an undefined label in ordinary
			// code, so this is the case it does not see: a jump INTO a statement
			// expression, which gcc rejects as "jump into statement expression" and
			// modernc accepts.
			name: "goto into a statement expression",
			src: `int f(int a){
				int x = ({ int t = a; inner: t; });
				if (x < 5) goto inner;
				return x;
			}`,
			want: "no such label",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := cc.Compile("x.c", c.src)
			require.Error(t, err, "must not compile silently")
			require.Contains(t, err.Error(), c.want)
		})
	}
}

// The constructs that do fold still work, including the ones next to the paths
// above: a goto forward to a label defined later, and case labels from constant
// expressions and enum constants.
func TestFoldableConstructsStillWork(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
enum { TWO = 2, TEN = 10 };
static int classify(int n){
	switch(n){
	case 1:        return 100;
	case TWO:      return 200;        /* an enum constant */
	case 3 + 4:    return 700;        /* a constant expression */
	case TEN ... TEN + 2: return 999; /* a GNU range */
	default:       return 0;
	}
}
int main(void){
	int i = 0;
	goto forward;                     /* a label defined later */
back:
	printf("%d %d %d %d %d %zu\n", classify(1), classify(2), classify(7),
	       classify(11), classify(50), sizeof(int));
	return 0;
forward:
	i = 1;
	if (i) goto back;
	return 1;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "100 200 700 999 0 4\n", out)
}
