package cc_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// C has no garbage collector, so nothing this front end emits may be marked as a
// collector root.
//
// The marks are not the front end's doing: ir.Block.Load flags every ClsP load as
// a GC reference, which is what goc needs and is meaningless here. Leaving them
// is expensive rather than merely untidy -- the register allocator pins a GC
// reference that is live across a call into a fixed stack slot for the whole of
// its life, so the collector can find it at a known frame offset. With no
// collector that is a value evicted from a register for nobody, and in SQLite's
// sqlite3VdbeExec it was 67 of them, the database handle among them.
//
// The check runs after the optimiser, not just after the front end, because the
// front end clearing them is only half an argument: a later pass that mints a
// managed phi would put them back. opt/mem2reg.go does exactly that for a slot it
// considers managed, and the reason it does not here is that a C pointer is ClsP
// rather than the managed class -- which is a property worth a test rather than a
// comment, since it is two packages away from the code that depends on it.
func TestCompiledCHasNoGCReferences(t *testing.T) {
	const src = `
#include <stdlib.h>
#include <string.h>
struct node { struct node *next; char *name; int v; };
extern int sink(void *);
extern void *make(void);

/* Pointer locals live across calls: exactly the shape that gets pinned. A
   pointer stored, reloaded, and used after a call is what marks a temp. */
int walk(struct node *head, int n) {
	struct node *cur = head;
	char *label = NULL;
	void *scratch = make();
	int total = 0;
	while (cur && n-- > 0) {
		label = cur->name;
		total += sink(cur);        /* cur and label are live across this */
		total += (int)strlen(label);
		total += sink(scratch);    /* scratch is live across the whole loop */
		cur = cur->next;
	}
	free(scratch);
	return total;
}
`
	module, err := cc.Compile("main.c", src)
	require.NoError(t, err)
	opt.OptimizeModule(module)

	for _, f := range module.Funcs {
		for id, temp := range f.Temps {
			if temp == nil {
				continue
			}
			require.Falsef(t, temp.GCRef,
				"function %s temp %d (%q, class %v) is marked as a GC root; C has no collector, "+
					"and the register allocator pins such a value into a stack slot for its whole life",
				f.Name, id, temp.Name, temp.Cls)
			require.Zerof(t, temp.GCType,
				"function %s temp %d (%q) carries a GC type", f.Name, id, temp.Name)
		}
		// Guard the premise as well: if this stopped emitting pointer temps the
		// check above would pass while testing nothing.
		if f.Name == "walk" {
			pointers := 0
			for _, temp := range f.Temps {
				if temp != nil && temp.Cls == ir.ClsP {
					pointers++
				}
			}
			require.NotZero(t, pointers, "walk should still have pointer temps to be worth checking")
		}
	}
}
