package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
)

// fakeBudget stands in for the backend. Frame is a function of the body's
// instruction count so a test can say "this much inlining costs this many
// bytes" without a register allocator.
type fakeBudget struct {
	headroom       map[string]int
	bytesPerInstr  int
	measurements   int
	measureFailure map[string]bool
}

func (b *fakeBudget) Headroom(name string) int { return b.headroom[name] }
func (b *fakeBudget) Symbol(f *ir.Func) string { return f.Name }

func (b *fakeBudget) Frame(f *ir.Func) (int, error) {
	b.measurements++
	if b.measureFailure[f.Name] {
		return 0, errMeasure
	}
	size := 0
	for _, block := range f.Blocks {
		size += len(block.Instrs)
	}
	return size * b.bytesPerInstr, nil
}

var errMeasure = &measureError{}

type measureError struct{}

func (*measureError) Error() string { return "cannot lay this out" }

func withBudget(t *testing.T, budget *fakeBudget) {
	t.Helper()
	previous := NoSplitFrameBudgetFor
	NoSplitFrameBudgetFor = func(*ir.Module) (FrameBudget, error) { return budget, nil }
	t.Cleanup(func() { NoSplitFrameBudgetFor = previous })
}

// noSplitCallerModule builds a nosplit caller with `calls` calls to a small
// splittable helper, which is what the pass would inline.
func noSplitCallerModule(calls int) (*ir.Module, *ir.Func) {
	m := ir.NewModule()

	// The helper reads and writes through a pointer, so its body survives the
	// cleanup the pass runs before it measures. A helper that folds away would
	// make every measurement zero and every bound trivially satisfied.
	helper := m.NewFunc("helper", ir.ClsL)
	pointer := helper.Param("p", ir.ClsP)
	body := helper.Entry()
	loaded := body.Load(ir.ClsL, pointer)
	doubled := body.Add(ir.ClsL, loaded, loaded)
	body.Store(doubled, pointer)
	body.Ret(body.Add(ir.ClsL, doubled, body.Load(ir.ClsL, body.Add(ir.ClsP, pointer, helper.ConstInt(ir.ClsP, 8)))))

	caller := m.NewFunc("caller", ir.ClsL)
	caller.ManagedFrame = true
	caller.NoSplit = true
	seed := caller.Param("p", ir.ClsP)
	entry := caller.Entry()
	total := caller.ConstInt(ir.ClsL, 0)
	for index := 0; index < calls; index++ {
		total = entry.Add(ir.ClsL, total, entry.Call(ir.ClsL, caller.Sym("helper", 0), seed))
	}
	entry.Ret(total)
	return m, caller
}

func bodySize(f *ir.Func) int {
	size := 0
	for _, block := range f.Blocks {
		size += len(block.Instrs)
	}
	return size
}

func TestNoSplitInlineIsSkippedWithoutABackend(t *testing.T) {
	previous := NoSplitFrameBudgetFor
	NoSplitFrameBudgetFor = nil
	t.Cleanup(func() { NoSplitFrameBudgetFor = previous })

	m, caller := noSplitCallerModule(3)
	before := caller.String()
	if InlineIntoNoSplitCallers(m) {
		t.Fatal("inlined into a nosplit caller with no frame budget to bound it")
	}
	if caller.String() != before {
		t.Error("the caller was changed anyway")
	}
}

func TestNoSplitInlineRefusesACallerWithNoHeadroom(t *testing.T) {
	budget := &fakeBudget{headroom: map[string]int{"caller": 0}, bytesPerInstr: 8}
	withBudget(t, budget)

	m, caller := noSplitCallerModule(3)
	before := caller.String()
	report, changed := InlineIntoNoSplitCallersReporting(m)
	if changed {
		t.Fatal("inlined into a caller with no headroom")
	}
	if caller.String() != before {
		t.Error("the caller was changed anyway")
	}
	if len(report.NoRoom) != 1 || report.NoRoom[0] != "caller" {
		t.Errorf("NoRoom = %v, want [caller]", report.NoRoom)
	}
	if budget.measurements != 0 {
		t.Errorf("measured %d frames for a caller it could not touch", budget.measurements)
	}
}

// Negative headroom is what a chain that is already over the reserve reports,
// and it must be treated as strictly worse than none.
func TestNoSplitInlineRefusesACallerAlreadyOverTheReserve(t *testing.T) {
	budget := &fakeBudget{headroom: map[string]int{"caller": -184}, bytesPerInstr: 8}
	withBudget(t, budget)

	m, caller := noSplitCallerModule(3)
	before := caller.String()
	if InlineIntoNoSplitCallers(m) {
		t.Fatal("inlined into a caller whose chain is already over the reserve")
	}
	if caller.String() != before {
		t.Error("the caller was changed anyway")
	}
}

func TestNoSplitInlineAcceptsAFrameThatFits(t *testing.T) {
	budget := &fakeBudget{headroom: map[string]int{"caller": 4096}, bytesPerInstr: 8}
	withBudget(t, budget)

	m, caller := noSplitCallerModule(3)
	before := bodySize(caller)
	report, changed := InlineIntoNoSplitCallersReporting(m)
	if !changed {
		t.Fatal("nothing was inlined into a caller with 4096 bytes of headroom")
	}
	if len(report.Accepted) != 1 || report.Accepted[0].Name != "caller" {
		t.Fatalf("Accepted = %+v, want one entry for caller", report.Accepted)
	}
	if bodySize(caller) <= before {
		t.Errorf("the caller's body did not grow: %d -> %d", before, bodySize(caller))
	}
	for _, block := range caller.Blocks {
		for index := range block.Instrs {
			if block.Instrs[index].Op == ir.OCall {
				t.Error("a call to helper survived")
			}
		}
	}
}

// The bound has to actually bind, and a caller that fails it has to come back
// exactly as it was.
func TestNoSplitInlineRevertsAFrameThatDoesNot(t *testing.T) {
	budget := &fakeBudget{headroom: map[string]int{"caller": 24}, bytesPerInstr: 64}
	withBudget(t, budget)

	m, caller := noSplitCallerModule(4)
	before := caller.String()
	report, changed := InlineIntoNoSplitCallersReporting(m)
	if changed {
		t.Fatal("kept an inlining that measured past its allowance")
	}
	if got := caller.String(); got != before {
		t.Errorf("the caller was not restored:\n--- before\n%s\n--- after\n%s", before, got)
	}
	if len(report.Rejected) != 1 {
		t.Fatalf("Rejected = %+v, want one entry", report.Rejected)
	}
	if report.Rejected[0].After-report.Rejected[0].Before <= report.Rejected[0].Allowance {
		t.Errorf("rejected a growth that was within the allowance: %+v", report.Rejected[0])
	}
}

// A backend that cannot lay the caller out is not a licence to inline anyway.
func TestNoSplitInlineRevertsWhenTheMeasurementFails(t *testing.T) {
	budget := &fakeBudget{
		headroom:       map[string]int{"caller": 4096},
		bytesPerInstr:  8,
		measureFailure: map[string]bool{},
	}
	withBudget(t, budget)

	m, caller := noSplitCallerModule(3)
	before := caller.String()
	// Let the "before" measurement succeed and the "after" one fail.
	failAfterFirst := &failingAfterFirstBudget{fakeBudget: budget}
	NoSplitFrameBudgetFor = func(*ir.Module) (FrameBudget, error) { return failAfterFirst, nil }

	if InlineIntoNoSplitCallers(m) {
		t.Fatal("kept an inlining whose result could not be measured")
	}
	if got := caller.String(); got != before {
		t.Errorf("the caller was not restored:\n--- before\n%s\n--- after\n%s", before, got)
	}
}

type failingAfterFirstBudget struct {
	*fakeBudget
	calls int
}

func (b *failingAfterFirstBudget) Frame(f *ir.Func) (int, error) {
	b.calls++
	if b.calls > 1 {
		return 0, errMeasure
	}
	return b.fakeBudget.Frame(f)
}

// A splittable caller is not this pass's business; the ordinary inliner has
// already had it.
func TestNoSplitInlineLeavesSplittableCallersAlone(t *testing.T) {
	budget := &fakeBudget{headroom: map[string]int{"caller": 4096}, bytesPerInstr: 8}
	withBudget(t, budget)

	m, caller := noSplitCallerModule(3)
	caller.NoSplit = false
	before := caller.String()
	if InlineIntoNoSplitCallers(m) {
		t.Fatal("the nosplit pass touched a splittable caller")
	}
	if caller.String() != before {
		t.Error("the caller was changed anyway")
	}
}

func TestNoSplitInlineIsDisabledByEnvironment(t *testing.T) {
	budget := &fakeBudget{headroom: map[string]int{"caller": 4096}, bytesPerInstr: 8}
	withBudget(t, budget)
	t.Setenv("GOC_NO_NOSPLIT_INLINE", "1")

	m, caller := noSplitCallerModule(3)
	before := caller.String()
	if InlineIntoNoSplitCallers(m) {
		t.Fatal("the bisection switch did not disable the pass")
	}
	if caller.String() != before {
		t.Error("the caller was changed anyway")
	}
}
