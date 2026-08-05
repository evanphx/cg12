package stackcheck

import (
	"strings"
	"testing"
)

// linux/arm64's numbers, so the tests read against the same arithmetic the
// backend uses.
func config() Config {
	return Config{Limit: 920, CallSize: 0}
}

func check(t *testing.T, funcs []Func, cfg Config) (*Report, error) {
	t.Helper()
	report, err := Check(funcs, cfg)
	if report == nil {
		t.Fatal("Check returned no report")
	}
	return report, err
}

func TestChainWithinLimitIsAccepted(t *testing.T) {
	funcs := []Func{
		{Name: "entry", Frame: 64, Calls: []string{"a"}},
		{Name: "a", Frame: 400, NoSplit: true, Calls: []string{"b"}},
		{Name: "b", Frame: 400, NoSplit: true},
	}
	report, err := check(t, funcs, config())
	if err != nil {
		t.Fatalf("800-byte chain rejected against a 920-byte limit: %v", err)
	}
	if report.Height != 800 {
		t.Errorf("height = %d, want 800", report.Height)
	}
	if got := strings.Join(report.Deepest, " -> "); got != "a -> b" {
		t.Errorf("deepest = %q, want %q", got, "a -> b")
	}
}

func TestChainOverLimitIsRejected(t *testing.T) {
	funcs := []Func{
		{Name: "a", Frame: 480, NoSplit: true, Calls: []string{"b"}},
		{Name: "b", Frame: 480, NoSplit: true},
	}
	_, err := check(t, funcs, config())
	if err == nil {
		t.Fatal("960-byte chain accepted against a 920-byte limit")
	}
	message := err.Error()
	for _, want := range []string{"a -> b", "960 bytes", "920-byte limit", "40 over", "480  a", "480  b"} {
		if !strings.Contains(message, want) {
			t.Errorf("error does not mention %q:\n%s", want, message)
		}
	}
}

// A splittable callee ends the chain: it proves its own frame fits at entry.
func TestSplittableCalleeEndsTheChain(t *testing.T) {
	funcs := []Func{
		{Name: "a", Frame: 800, NoSplit: true, Calls: []string{"big"}},
		{Name: "big", Frame: 4096},
	}
	report, err := check(t, funcs, config())
	if err != nil {
		t.Fatalf("chain ending in a splittable callee rejected: %v", err)
	}
	if report.Height != 800 {
		t.Errorf("height = %d, want 800 (the splittable callee's 4096 must not count)", report.Height)
	}
}

func TestSelfRecursionIsUnbounded(t *testing.T) {
	funcs := []Func{{Name: "loop", Frame: 16, NoSplit: true, Calls: []string{"loop"}}}
	_, err := check(t, funcs, config())
	if err == nil {
		t.Fatal("a self-recursive nosplit function was accepted")
	}
	message := err.Error()
	if !strings.Contains(message, "unbounded nosplit stack") || !strings.Contains(message, "cycle") {
		t.Errorf("error does not report a cycle:\n%s", message)
	}
	if !strings.Contains(message, "loop") {
		t.Errorf("error does not name the function:\n%s", message)
	}
}

func TestMutualRecursionIsUnboundedAndReportedOnce(t *testing.T) {
	funcs := []Func{
		{Name: "ping", Frame: 16, NoSplit: true, Calls: []string{"pong"}},
		{Name: "pong", Frame: 16, NoSplit: true, Calls: []string{"ping"}},
	}
	_, err := check(t, funcs, config())
	if err == nil {
		t.Fatal("mutually recursive nosplit functions were accepted")
	}
	failure, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is %T, want *Error", err)
	}
	if len(failure.Chains) != 1 {
		t.Fatalf("reported %d chains for one cycle, want 1", len(failure.Chains))
	}
	if !failure.Chains[0].Unbounded || failure.Chains[0].Cycle == "" {
		t.Errorf("chain is not reported as a cycle: %+v", failure.Chains[0])
	}
}

// A cycle that a splittable function breaks is not a cycle: the chain restarts
// at the splittable function, so neither side of it is unbounded.
func TestCycleBrokenByASplittableFunctionIsBounded(t *testing.T) {
	funcs := []Func{
		{Name: "a", Frame: 16, NoSplit: true, Calls: []string{"guarded"}},
		{Name: "guarded", Frame: 4096, Calls: []string{"a"}},
	}
	report, err := check(t, funcs, config())
	if err != nil {
		t.Fatalf("chain through a splittable function rejected: %v", err)
	}
	if report.Height != 16 {
		t.Errorf("height = %d, want 16", report.Height)
	}
}

// A function reached from a cycle, but not part of it, keeps its own height --
// the sentinel that marks the cycle must not leak downward into it. Its
// headroom is a different question and is correctly gone: every chain that
// reaches it comes through the cycle.
func TestFunctionBelowACycleKeepsItsOwnHeight(t *testing.T) {
	funcs := []Func{
		{Name: "a", Frame: 16, NoSplit: true, Calls: []string{"b"}},
		{Name: "b", Frame: 16, NoSplit: true, Calls: []string{"a", "leaf"}},
		{Name: "leaf", Frame: 48, NoSplit: true},
	}
	report, err := check(t, funcs, config())
	failure, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is %T, want *Error", err)
	}
	for _, chain := range failure.Chains {
		for _, frame := range chain.Frames {
			if frame.Name == "leaf" {
				t.Errorf("leaf is reported as part of the cycle:\n%s", err)
			}
		}
	}
	if report.Headroom["leaf"] >= 0 {
		t.Errorf("leaf headroom = %d, want negative: a cycle sits above it", report.Headroom["leaf"])
	}
}

func TestDynamicAllocationIsUnbounded(t *testing.T) {
	funcs := []Func{{Name: "vla", Frame: 32, NoSplit: true, DynamicAlloc: true}}
	_, err := check(t, funcs, config())
	if err == nil {
		t.Fatal("a nosplit frame with a dynamic stack allocation was accepted")
	}
	if !strings.Contains(err.Error(), "dynamic stack allocation") {
		t.Errorf("error does not say why it is unbounded:\n%s", err)
	}
}

// The default indirect policy is Go's: the target checks its own stack.
func TestIndirectCallEndsTheChainByDefault(t *testing.T) {
	funcs := []Func{
		{Name: "dispatch", Frame: 500, NoSplit: true, Indirect: true},
		{Name: "target", Frame: 500, NoSplit: true, AddressTaken: true},
	}
	report, err := check(t, funcs, config())
	if err != nil {
		t.Fatalf("indirect call rejected under the default policy: %v", err)
	}
	if report.Height != 500 {
		t.Errorf("height = %d, want 500", report.Height)
	}
	if len(report.IndirectFromNoSplit) != 1 || report.IndirectFromNoSplit[0] != "dispatch" {
		t.Errorf("indirect callers = %v, want [dispatch]", report.IndirectFromNoSplit)
	}
	if len(report.AddressTakenNoSplit) != 1 || report.AddressTakenNoSplit[0] != "target" {
		t.Errorf("address-taken nosplit = %v, want [target]", report.AddressTakenNoSplit)
	}
}

func TestStrictIndirectResolvesAgainstAddressTakenFunctions(t *testing.T) {
	funcs := []Func{
		{Name: "dispatch", Frame: 500, NoSplit: true, Indirect: true},
		{Name: "target", Frame: 500, NoSplit: true, AddressTaken: true},
	}
	cfg := config()
	cfg.StrictIndirect = true
	_, err := check(t, funcs, cfg)
	if err == nil {
		t.Fatal("strict mode accepted a 1000-byte indirect chain")
	}
	if !strings.Contains(err.Error(), "indirect call") {
		t.Errorf("error does not mark the indirect edge:\n%s", err)
	}
}

// With no address-taken nosplit function, the default policy's assumption is a
// theorem rather than an assumption, and strict mode agrees with it.
func TestStrictIndirectIsFreeWhenNoNoSplitFunctionIsAddressTaken(t *testing.T) {
	funcs := []Func{
		{Name: "dispatch", Frame: 900, NoSplit: true, Indirect: true},
		{Name: "target", Frame: 900, NoSplit: true},
	}
	cfg := config()
	cfg.StrictIndirect = true
	report, err := check(t, funcs, cfg)
	if err != nil {
		t.Fatalf("strict mode rejected an indirect call with no possible nosplit target: %v", err)
	}
	if report.Height != 900 {
		t.Errorf("height = %d, want 900", report.Height)
	}
}

// An assembly function's frame is known even though its body is not compiled
// here, and it must be charged.
func TestAssemblyFrameIsCharged(t *testing.T) {
	funcs := []Func{
		{Name: "go_caller", Frame: 500, NoSplit: true, Calls: []string{"asm_helper"}},
		{Name: "asm_helper", Frame: 480, NoSplit: true, Opaque: true},
	}
	report, err := check(t, funcs, config())
	if err == nil {
		t.Fatal("980-byte chain through assembly accepted")
	}
	if !strings.Contains(err.Error(), "asm_helper") {
		t.Errorf("error does not name the assembly function:\n%s", err)
	}
	if len(report.Opaque) != 1 || report.Opaque[0] != "asm_helper" {
		t.Errorf("opaque = %v, want [asm_helper]", report.Opaque)
	}
}

func TestUndefinedCalleeEndsTheChainAndIsReported(t *testing.T) {
	funcs := []Func{{Name: "a", Frame: 900, NoSplit: true, Calls: []string{"somewhere_else"}}}
	report, err := check(t, funcs, config())
	if err != nil {
		t.Fatalf("chain ending at an undefined symbol rejected: %v", err)
	}
	if len(report.External) != 1 || report.External[0] != "somewhere_else" {
		t.Errorf("external = %v, want [somewhere_else]", report.External)
	}
}

func TestUncheckedSplittableChainEndIsReported(t *testing.T) {
	funcs := []Func{
		{Name: "a", Frame: 900, NoSplit: true, Calls: []string{"asm_wrapper"}},
		{Name: "asm_wrapper", Frame: 4096, Unchecked: true},
	}
	report, err := check(t, funcs, config())
	if err != nil {
		t.Fatalf("chain ending at an unchecked splittable function rejected: %v", err)
	}
	if len(report.Unchecked) != 1 || report.Unchecked[0] != "asm_wrapper" {
		t.Errorf("unchecked = %v, want [asm_wrapper]", report.Unchecked)
	}
}

// Only the chain's root is reported: an over-limit function that is only ever
// reached through another one is already named by the surviving chain.
func TestOnlyRootsAreReported(t *testing.T) {
	funcs := []Func{
		{Name: "root", Frame: 480, NoSplit: true, Calls: []string{"middle"}},
		{Name: "middle", Frame: 480, NoSplit: true, Calls: []string{"leaf"}},
		{Name: "leaf", Frame: 480, NoSplit: true},
	}
	_, err := check(t, funcs, config())
	failure, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is %T, want *Error", err)
	}
	if len(failure.Chains) != 1 {
		t.Fatalf("reported %d chains, want 1", len(failure.Chains))
	}
	if got := len(failure.Chains[0].Frames); got != 3 {
		t.Errorf("chain has %d frames, want 3", got)
	}
}

func TestRecordedDebtAllowsItsOwnHeightAndNotOneByteMore(t *testing.T) {
	funcs := []Func{
		{Name: "known", Frame: 500, NoSplit: true, Calls: []string{"tail"}},
		{Name: "tail", Frame: 500, NoSplit: true},
	}
	cfg := config()
	cfg.Recorded = map[string]int{"known": 1000}
	if _, err := check(t, funcs, cfg); err != nil {
		t.Fatalf("a chain at its recorded height was rejected: %v", err)
	}

	funcs[0].Frame = 516 // one 16-byte slot more
	if _, err := check(t, funcs, cfg); err == nil {
		t.Fatal("a recorded chain that grew past its recorded height was accepted")
	}
}

// The register is debt, not an allowance: it never widens the headroom a pass is
// told it may spend.
func TestRecordedDebtDoesNotWidenHeadroom(t *testing.T) {
	funcs := []Func{{Name: "known", Frame: 1000, NoSplit: true}}
	cfg := config()
	cfg.Recorded = map[string]int{"known": 1000}
	report, err := check(t, funcs, cfg)
	if err != nil {
		t.Fatalf("recorded chain rejected: %v", err)
	}
	if got := report.Headroom["known"]; got != 920-1000 {
		t.Errorf("headroom = %d, want %d", got, 920-1000)
	}
}

func TestHeadroomAccountsForFramesAboveAndBelow(t *testing.T) {
	funcs := []Func{
		{Name: "top", Frame: 200, NoSplit: true, Calls: []string{"middle"}},
		{Name: "middle", Frame: 100, NoSplit: true, Calls: []string{"bottom"}},
		{Name: "bottom", Frame: 300, NoSplit: true},
	}
	report, err := check(t, funcs, config())
	if err != nil {
		t.Fatalf("600-byte chain rejected: %v", err)
	}
	// Every function on this one chain sees the same 920-600 slack, whichever
	// end of it the function sits at.
	for _, name := range []string{"top", "middle", "bottom"} {
		if got := report.Headroom[name]; got != 320 {
			t.Errorf("headroom[%s] = %d, want 320", name, got)
		}
	}
}

// The deepest chain through a function may not be the one that starts at it.
func TestHeadroomTakesTheDeepestChainThroughAFunction(t *testing.T) {
	funcs := []Func{
		{Name: "shallow", Frame: 16, NoSplit: true, Calls: []string{"shared"}},
		{Name: "deep", Frame: 600, NoSplit: true, Calls: []string{"shared"}},
		{Name: "shared", Frame: 100, NoSplit: true},
	}
	report, err := check(t, funcs, config())
	if err != nil {
		t.Fatalf("700-byte chain rejected: %v", err)
	}
	if got := report.Headroom["shared"]; got != 920-700 {
		t.Errorf("headroom[shared] = %d, want %d: the 600-byte caller must be charged", got, 920-700)
	}
	if got := report.Headroom["shallow"]; got != 920-116 {
		t.Errorf("headroom[shallow] = %d, want %d", got, 920-116)
	}
}

func TestCallSizeIsChargedPerCall(t *testing.T) {
	funcs := []Func{
		{Name: "a", Frame: 100, NoSplit: true, Calls: []string{"b"}},
		{Name: "b", Frame: 100, NoSplit: true},
	}
	cfg := config()
	cfg.CallSize = 8
	report, err := check(t, funcs, cfg)
	if err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
	if report.Height != 208 {
		t.Errorf("height = %d, want 208 (100 + 8 for the call + 100)", report.Height)
	}
}

// Two definitions of one symbol keep the larger frame: the budget's job is a
// worst case, not an inventory.
func TestDuplicateDefinitionKeepsTheLargerFrame(t *testing.T) {
	funcs := []Func{
		{Name: "dup", Frame: 100, NoSplit: true},
		{Name: "dup", Frame: 950, NoSplit: true},
	}
	if _, err := check(t, funcs, config()); err == nil {
		t.Fatal("the larger of two definitions was ignored")
	}
}

func TestEmptyModuleIsAccepted(t *testing.T) {
	report, err := Check(nil, config())
	if err != nil {
		t.Fatalf("empty module rejected: %v", err)
	}
	if len(report.Deepest) != 0 || report.Height != 0 {
		t.Errorf("report = %+v, want an empty one", report)
	}
}

// The walk must not depend on the order the facts arrive in: the backend builds
// them from a concurrent emit loop.
func TestResultDoesNotDependOnInputOrder(t *testing.T) {
	forward := []Func{
		{Name: "a", Frame: 480, NoSplit: true, Calls: []string{"b"}},
		{Name: "b", Frame: 480, NoSplit: true, Calls: []string{"c"}},
		{Name: "c", Frame: 480, NoSplit: true},
	}
	reversed := []Func{forward[2], forward[1], forward[0]}
	_, first := check(t, forward, config())
	_, second := check(t, reversed, config())
	if first == nil || second == nil {
		t.Fatal("one order was accepted and the other rejected")
	}
	if first.Error() != second.Error() {
		t.Errorf("different errors for different input orders:\n%s\n---\n%s", first, second)
	}
}
