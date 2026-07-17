package testenv

import (
	"os"
	"runtime"
	"testing"
)

// Tool skips when a tool is absent, so a developer with a partial toolchain can
// still run the suite -- and fails when CG12_REQUIRE_TOOLS says the run is
// supposed to be checking everything.
func TestToolSkipsOrFailsByEnvironment(t *testing.T) {
	const absent = "cg12-no-such-tool-anywhere"

	t.Setenv("CG12_REQUIRE_TOOLS", "")
	if got := runFake(t, func(tb testing.TB) { Tool(tb, absent) }); got != "skip" {
		t.Errorf("without the env var, a missing tool should skip, got %s", got)
	}

	t.Setenv("CG12_REQUIRE_TOOLS", "1")
	if got := runFake(t, func(tb testing.TB) { Tool(tb, absent) }); got != "fatal" {
		t.Errorf("with CG12_REQUIRE_TOOLS, a missing tool should fail, got %s", got)
	}
}

// A tool that is present is returned either way.
func TestToolFindsWhatIsThere(t *testing.T) {
	for _, v := range []string{"", "1"} {
		t.Setenv("CG12_REQUIRE_TOOLS", v)
		if p := Tool(t, "go"); p == "" {
			t.Fatalf("go is on PATH but Tool returned %q (CG12_REQUIRE_TOOLS=%q)", p, v)
		}
	}
}

// The env var is off for an empty value, "0", or "false", so that setting it to
// nothing does not silently arm it.
func TestRequiredReading(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "false": false, "FALSE": false, "1": true, "yes": true} {
		t.Setenv("CG12_REQUIRE_TOOLS", v)
		if got := required(); got != want {
			t.Errorf("CG12_REQUIRE_TOOLS=%q: required()=%v, want %v", v, got, want)
		}
	}
	os.Unsetenv("CG12_REQUIRE_TOOLS")
	if required() {
		t.Error("unset should not require tools")
	}
}

// fakeTB records which of Skip/Fatal a helper reached, since a real testing.T
// cannot be asked after the fact.
type fakeTB struct {
	testing.TB
	outcome string
	done    chan struct{}
}

func (f *fakeTB) Helper() {}
func (f *fakeTB) Skipf(string, ...any) {
	f.outcome = "skip"
	close(f.done)
	runtime.Goexit()
}
func (f *fakeTB) Fatalf(string, ...any) {
	f.outcome = "fatal"
	close(f.done)
	runtime.Goexit()
}

func runFake(t *testing.T, fn func(testing.TB)) string {
	t.Helper()
	f := &fakeTB{done: make(chan struct{})}
	go func() {
		defer func() { recover() }()
		fn(f)
	}()
	<-f.done
	return f.outcome
}
