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
			// collectLabels walks compound, labelled, selection and iteration
			// statements -- not expression statements, so a label inside a GNU
			// statement expression never gets a block. The parser accepts the goto,
			// so nothing upstream catches it, and emitting nothing for a jump means
			// falling through to whatever follows instead.
			name: "goto a label inside a statement expression",
			src:  `int f(int a){ int x = ({ if (a) goto deep; a; deep: a + 1; }); return x; }`,
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
