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

	// chains are the sets of functions that share a nosplit chain, declared
	// rather than derived: which functions lie on a chain together is the
	// backend's question, and this file's is what the pass does once it is
	// told. Charging any member charges every member, which is what the arm64
	// budget does with the call graph.
	chains  [][]string
	charged map[string]int
}

func (b *fakeBudget) Headroom(name string) int { return b.headroom[name] - b.charged[name] }
func (b *fakeBudget) Symbol(f *ir.Func) string { return f.Name }

func (b *fakeBudget) Charge(name string, bytes int) {
	if bytes <= 0 {
		return
	}
	if b.charged == nil {
		b.charged = map[string]int{}
	}
	b.charged[name] += bytes
	for _, chain := range b.chains {
		if !chainContains(chain, name) {
			continue
		}
		for _, other := range chain {
			if other != name {
				b.charged[other] += bytes
			}
		}
	}
}

func chainContains(chain []string, name string) bool {
	for _, member := range chain {
		if member == name {
			return true
		}
	}
	return false
}

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

// noSplitChainModule builds two independent nosplit callers, each with `calls`
// calls to the same splittable helper. They do not call each other: whether they
// lie on one chain is the budget's judgement, and the fake budget states it, so
// the test is about the pass's arithmetic and not about a call graph.
func noSplitChainModule(calls int) (*ir.Module, *ir.Func, *ir.Func) {
	m, first := noSplitCallerModule(calls)
	first.Name = "first"

	second := m.NewFunc("second", ir.ClsL)
	second.ManagedFrame = true
	second.NoSplit = true
	seed := second.Param("p", ir.ClsP)
	entry := second.Entry()
	total := second.ConstInt(ir.ClsL, 0)
	for index := 0; index < calls; index++ {
		total = entry.Add(ir.ClsL, total, entry.Call(ir.ClsL, second.Sym("helper", 0), seed))
	}
	entry.Ret(total)
	return m, first, second
}

// Headroom belongs to a chain, not to a function. Two nosplit functions on one
// chain are each told the chain's whole remaining slack, so a pass that measures
// the budget once and then walks every caller can hand the same bytes out twice
// and grow the chain by double what it had. This is the caveat the wave-10 gate
// flagged as structural and not observed firing.
func TestNoSplitInlineDoesNotSpendOneChainsHeadroomTwice(t *testing.T) {
	// What one caller's inlining costs, measured rather than assumed, so the
	// test does not depend on the inliner's exact output.
	probe := &fakeBudget{headroom: map[string]int{"first": 1 << 20, "second": 0}, bytesPerInstr: 8}
	withBudget(t, probe)
	probeModule, _, _ := noSplitChainModule(3)
	probeReport, _ := InlineIntoNoSplitCallersReporting(probeModule)
	if len(probeReport.Accepted) != 1 {
		t.Fatalf("probe accepted %d callers, want 1: %+v", len(probeReport.Accepted), probeReport.Accepted)
	}
	growth := probeReport.Accepted[0].After - probeReport.Accepted[0].Before
	if growth <= noSplitInlineSafetyBytes {
		t.Fatalf("the probe's caller grew by %d bytes, too little for this test to bind", growth)
	}

	// Room for one such growth and no more. Before charging, both callers were
	// offered `room` and both fitted, so the chain grew by 2*growth.
	room := growth + noSplitInlineSafetyBytes + 8
	budget := &fakeBudget{
		headroom:      map[string]int{"first": room, "second": room},
		bytesPerInstr: 8,
		chains:        [][]string{{"first", "second"}},
	}
	withBudget(t, budget)

	m, _, _ := noSplitChainModule(3)
	report, changed := InlineIntoNoSplitCallersReporting(m)
	if !changed {
		t.Fatal("nothing was inlined at all; the test no longer exercises the bound")
	}
	if len(report.Accepted) != 1 {
		t.Fatalf("accepted %d callers on one chain with room for one: %+v", len(report.Accepted), report.Accepted)
	}
	spent := 0
	for _, result := range report.Accepted {
		spent += result.After - result.Before
	}
	if spent > room-noSplitInlineSafetyBytes {
		t.Errorf("grew the chain by %d bytes against %d of headroom", spent, room-noSplitInlineSafetyBytes)
	}
	if len(report.Rejected)+len(report.NoRoom) != 1 {
		t.Errorf("the second caller was neither rejected nor out of room: %+v / %v", report.Rejected, report.NoRoom)
	}
}

// The charge has to follow the chain, not the name: a caller that grows must
// cost the *other* function on its chain, or the accounting is per-function
// again under another name.
func TestNoSplitInlineChargesTheWholeChain(t *testing.T) {
	budget := &fakeBudget{
		headroom:      map[string]int{"first": 1 << 20, "second": 1 << 20},
		bytesPerInstr: 8,
		chains:        [][]string{{"first", "second"}},
	}
	withBudget(t, budget)

	m, _, _ := noSplitChainModule(3)
	report, _ := InlineIntoNoSplitCallersReporting(m)
	if len(report.Accepted) != 2 {
		t.Fatalf("accepted %d callers with a megabyte of headroom each: %+v", len(report.Accepted), report.Accepted)
	}
	total := 0
	for _, result := range report.Accepted {
		total += result.After - result.Before
	}
	if budget.charged["first"] != total || budget.charged["second"] != total {
		t.Errorf("charged first=%d second=%d, want %d on both", budget.charged["first"], budget.charged["second"], total)
	}
	// The second caller was offered what the first left, not what it started
	// with. Allowance is recorded per caller, so this is checkable.
	if report.Accepted[1].Allowance >= report.Accepted[0].Allowance {
		t.Errorf("the second caller's allowance did not shrink: %+v", report.Accepted)
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
