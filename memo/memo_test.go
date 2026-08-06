package memo

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

// buildTwo makes a module with a caller and a small callee, both with bodies the
// pipeline will actually touch.
func buildTwo(t *testing.T) (*ir.Module, *ir.Func, *ir.Func) {
	t.Helper()
	m := ir.NewModule()

	callee := m.NewFunc("callee", ir.ClsW)
	x := callee.Param("x", ir.ClsW)
	callee.Entry().Ret(callee.Entry().Add(ir.ClsW, x, callee.Word(1)))

	caller := m.NewFunc("caller", ir.ClsW).Export()
	y := caller.Param("y", ir.ClsW)
	entry := caller.Entry()
	entry.Ret(entry.Call(ir.ClsW, caller.Sym("callee", 0), y))
	return m, caller, callee
}

// TestFuncDigestIsTheBodyAndNotTheModule is the property that makes the memo
// key cheap and portable: a function's digest must not move when something
// elsewhere in the module does.
func TestFuncDigestIsTheBodyAndNotTheModule(t *testing.T) {
	m, caller, callee := buildTwo(t)
	before, _, err := FuncDigest(caller)
	if err != nil {
		t.Fatal(err)
	}
	// Grow the module's file table, which is what a per-function unit used to
	// carry a copy of.
	m.File("some/other/file.go")
	m.File("and/another.go")
	_ = callee
	after, _, err := FuncDigest(caller)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("caller's digest moved when the module's file table grew: %s -> %s", before, after)
	}
}

// TestRoundTripPreservesTheBody holds the property the whole memo rests on: a
// body that goes through the unit format and comes back is the same body.
func TestRoundTripPreservesTheBody(t *testing.T) {
	_, caller, _ := buildTwo(t)
	want, encoded, err := FuncDigest(caller)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalFunc(encoded, want)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := FuncDigest(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip changed the body: %s -> %s", want, got)
	}
}

// TestUnmarshalRejectsCorruption is the integrity check that stands in for the
// IR verifier, which cannot be used here (see ir.DecodeModuleUnverified).
func TestUnmarshalRejectsCorruption(t *testing.T) {
	_, caller, _ := buildTwo(t)
	want, encoded, err := FuncDigest(caller)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), encoded...)
	corrupt[len(corrupt)/2] ^= 0xff
	if _, err := UnmarshalFunc(corrupt, want); err == nil {
		t.Fatal("a corrupted unit decoded without complaint")
	}
}

// TestEntryValidityChecksEveryClause: the key is a list of clauses rather than
// one opaque hash precisely so that each can be checked, so each must be.
func TestEntryValidityChecksEveryClause(t *testing.T) {
	own := Digest{1}
	module := Digest{2}
	depDigest := Digest{3}
	entry := &Entry{
		Name: "caller", Input: own, Module: module, Recursive: false,
		Deps: []Dep{{Name: "callee", Input: depDigest, Recursive: false}},
	}
	inputs := map[string]Digest{"callee": depDigest}
	rec := map[string]bool{}

	if ok, why := entry.Valid(own, module, false, inputs, rec); !ok {
		t.Fatalf("an unchanged entry was rejected: %s", why)
	}
	if ok, _ := entry.Valid(Digest{9}, module, false, inputs, rec); ok {
		t.Fatal("a moved own input was accepted")
	}
	if ok, _ := entry.Valid(own, Digest{9}, false, inputs, rec); ok {
		t.Fatal("moved module facts were accepted")
	}
	if ok, _ := entry.Valid(own, module, true, inputs, rec); ok {
		t.Fatal("a changed own recursion classification was accepted")
	}
	if ok, _ := entry.Valid(own, module, false, map[string]Digest{"callee": {9}}, rec); ok {
		t.Fatal("a moved dependency was accepted")
	}
	if ok, _ := entry.Valid(own, module, false, map[string]Digest{}, rec); ok {
		t.Fatal("a dependency that left the module was accepted")
	}
	if ok, _ := entry.Valid(own, module, false, inputs, map[string]bool{"callee": true}); ok {
		t.Fatal("a dependency whose recursion classification changed was accepted -- this is the SCC hole")
	}
}

// TestStoreRoundTrip: the memo file has to come back as what went in, including
// the spliced set, which is not part of the key but decides the live set.
func TestStoreRoundTrip(t *testing.T) {
	store := NewStore()
	store.Data = Digest{7}
	store.Put(&Entry{
		Name: "f", Input: Digest{1}, Module: Digest{2}, Output: Digest{3},
		Recursive: true, Dropped: true, Body: []byte("body-bytes"),
		Deps:    []Dep{{Name: "g", Input: Digest{4}, Recursive: true}},
		Spliced: []string{"g", "h"},
	})
	path := t.TempDir() + "/memo"
	if err := store.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Data != store.Data {
		t.Fatalf("data digest: got %s want %s", loaded.Data, store.Data)
	}
	got := loaded.Lookup("f")
	if got == nil {
		t.Fatal("entry missing after a round trip")
	}
	want := store.Lookup("f")
	if got.Input != want.Input || got.Module != want.Module || got.Output != want.Output ||
		got.Recursive != want.Recursive || got.Dropped != want.Dropped || string(got.Body) != string(want.Body) {
		t.Fatalf("entry changed across the round trip: %+v", got)
	}
	if len(got.Deps) != 1 || got.Deps[0] != want.Deps[0] {
		t.Fatalf("deps changed: %+v", got.Deps)
	}
	if len(got.Spliced) != 2 || got.Spliced[0] != "g" || got.Spliced[1] != "h" {
		t.Fatalf("spliced set changed: %v", got.Spliced)
	}
}

// TestLoadOfAMissingFileIsAColdBuild: an absent cache is not an error.
func TestLoadOfAMissingFileIsAColdBuild(t *testing.T) {
	store, err := Load(t.TempDir() + "/does-not-exist")
	if err != nil {
		t.Fatalf("a missing memo file should be a cold build, got %v", err)
	}
	if len(store.Entries) != 0 {
		t.Fatalf("a missing memo file produced %d entries", len(store.Entries))
	}
}

// TestFrozenFunctionsAreNotTransformed holds the mechanism the whole memo is
// built on: opt.Session.Freeze must make every pass in the pipeline leave a
// function alone. It runs the same module twice so the control is real -- the
// unfrozen run has to move the function, or the frozen run proves nothing.
func TestFrozenFunctionsAreNotTransformed(t *testing.T) {
	digestOf := func(f *ir.Func) Digest {
		t.Helper()
		d, _, err := FuncDigest(f)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	loose, looseCaller, _ := buildTwo(t)
	before := digestOf(looseCaller)
	opt.NewSession().Run(loose, opt.DefaultPipeline())
	if digestOf(looseCaller) == before {
		t.Fatal("the control does not hold: the pipeline left the caller alone even unfrozen")
	}

	frozen, frozenCaller, _ := buildTwo(t)
	session := opt.NewSession()
	session.Freeze(map[*ir.Func]bool{frozenCaller: true})
	session.Run(frozen, opt.DefaultPipeline())
	if after := digestOf(frozenCaller); after != before {
		t.Fatalf("a frozen function was transformed: %s -> %s", before, after)
	}
}
