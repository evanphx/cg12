package goc

import (
	"go/constant"
	"go/token"
	"go/types"
	"runtime"
	"testing"
)

func TestParseTarget(t *testing.T) {
	for _, name := range []string{"arm64", "amd64"} {
		target, err := ParseTarget(name)
		if err != nil {
			t.Fatalf("ParseTarget(%q) failed: %v", name, err)
		}
		if target.GOARCH() != name {
			t.Errorf("ParseTarget(%q).GOARCH() = %q", name, target.GOARCH())
		}
	}
	for _, name := range []string{"", "riscv64", "ARM64", "linux/arm64"} {
		if target, err := ParseTarget(name); err == nil {
			t.Errorf("ParseTarget(%q) = %q, want an error", name, target)
		}
	}
}

// TestUnsetTargetIsTheHost pins the property the whole target refactor rests on:
// leaving the target unset must reproduce what goc did when it read
// runtime.GOARCH directly, so no existing caller changes behavior.
func TestUnsetTargetIsTheHost(t *testing.T) {
	if got := HostTarget(); got.GOARCH() != runtime.GOARCH {
		t.Errorf("HostTarget() = %q, want %q", got, runtime.GOARCH)
	}
	if got := Target("").resolve(); got != HostTarget() {
		t.Errorf("Target(\"\").resolve() = %q, want %q", got, HostTarget())
	}
	if got := TargetARM64.resolve(); got != TargetARM64 {
		t.Errorf("TargetARM64.resolve() = %q, want arm64", got)
	}
}

func TestTargetTypeGeometryIsShared(t *testing.T) {
	for _, target := range []Target{TargetARM64, TargetAMD64} {
		if err := checkTargetTypeSizes(target); err != nil {
			t.Errorf("checkTargetTypeSizes(%s) = %v, want nil", target, err)
		}
	}
	// A 32-bit target would lay pointers out differently from what the layout
	// helpers assume, and must be rejected rather than silently mislaid out.
	if err := checkTargetTypeSizes(Target("386")); err == nil {
		t.Error("checkTargetTypeSizes(386) = nil, want an error")
	}
}

// TestSourceLoaderSelectsTargetArchitectureFiles is the direct evidence that
// standard library file selection follows the target rather than the host.
// internal/goarch's GOARCH constant is defined once per architecture, in a file
// chosen by build tag and filename suffix, so the constant the type checker ends
// up with names whichever architecture the loader selected files for.
func TestSourceLoaderSelectsTargetArchitectureFiles(t *testing.T) {
	for _, target := range []Target{TargetARM64, TargetAMD64} {
		loader := newSourceLoader(token.NewFileSet(), target)
		pkg, err := loader.Import("internal/goarch")
		if err != nil {
			t.Fatalf("import internal/goarch for %s: %v", target, err)
		}
		declared, ok := pkg.Scope().Lookup("GOARCH").(*types.Const)
		if !ok {
			t.Fatalf("internal/goarch declares no GOARCH constant for %s", target)
		}
		selected := constant.StringVal(declared.Val())
		if selected != target.GOARCH() {
			t.Errorf("internal/goarch.GOARCH = %q for target %s, want %q", selected, target, target.GOARCH())
		}
	}
}
