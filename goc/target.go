package goc

import (
	"fmt"
	"go/types"
	"runtime"
)

// Target names the machine goc compiles Go for.
//
// goc used to read runtime.GOARCH directly, in a dozen places across the
// frontend and the driver, which made the host the target: there was no way to
// ask an x86-64 box for arm64 Go output or the reverse, and every "unsupported
// on %s" message named the machine running the compiler rather than the machine
// being compiled for. Target makes the choice explicit and cross-compilation
// expressible.
//
// This mirrors cc.Target (cc/compile.go), which solved the same problem for the
// C frontend, deliberately rather than inventing a second vocabulary: same
// spelling for the values, same "default when unset" behavior, same threading
// through an options struct.
//
// Target is the architecture only, as cc.Target is. The operating system stays
// the host's; RUNTIME_PLAN.md scopes the Go runtime work to Linux, so an OS axis
// would be a second decision with no current consumer.
type Target string

const (
	TargetARM64 Target = "arm64"
	TargetAMD64 Target = "amd64"
)

// HostTarget is the target matching the machine goc itself is running on.
//
// It is the default for every entry point that does not name a target, which is
// what keeps this refactor behavior-preserving: omitting the target reproduces
// goc's historical host-is-target behavior exactly.
func HostTarget() Target {
	return Target(runtime.GOARCH)
}

// ParseTarget converts a -target flag value or a GOARCH setting into a Target.
func ParseTarget(name string) (Target, error) {
	target := Target(name)
	switch target {
	case TargetARM64, TargetAMD64:
		return target, nil
	}
	return "", fmt.Errorf("goc: unknown target %q (arm64 | amd64)", name)
}

// GOARCH is the go/build GOARCH the target selects standard library files with.
// It is spelled as a method rather than used as a bare string conversion so the
// build-tag selection sites read as a deliberate target query.
func (t Target) GOARCH() string {
	return string(t)
}

// resolve returns the target to compile for, substituting the host when the
// caller left the target unset. Mirrors cc.CompileWith's treatment of an empty
// Options.Target.
func (t Target) resolve() Target {
	if t == "" {
		return HostTarget()
	}
	return t
}

// sizes is the go/types size model for the target.
func (t Target) sizes() types.Sizes {
	return types.SizesFor("gc", string(t))
}

// goTypeSizes is the go/types size model goc lays Go types out with.
//
// It is deliberately not parameterised by Target. Every target goc supports is
// LP64 -- types.SizesFor("gc", "arm64") and types.SizesFor("gc", "amd64") are
// both an 8-byte word with 8-byte maximum alignment, so they answer Sizeof,
// Alignof and Offsetsof identically -- while the helpers that consume the model
// (typeSize, typeAlign, pointerSize, structOffsets, representativeType,
// fieldOffset) are free functions reached from roughly 150 call sites, most of
// which have no target in hand and would each have to grow a parameter.
//
// checkTargetTypeSizes is what stops that from being a silent assumption:
// compile rejects a target whose gc size model differs from this one, so a
// future 32-bit or differently-aligned target fails with a diagnostic instead of
// being laid out with the wrong geometry.
func goTypeSizes() types.Sizes {
	return TargetARM64.sizes()
}

// checkTargetTypeSizes reports an error when target does not share the LP64 type
// geometry goTypeSizes assumes. See goTypeSizes for why that assumption exists.
func checkTargetTypeSizes(target Target) error {
	assumed := goTypeSizes()
	actual := target.sizes()
	if actual == nil {
		return fmt.Errorf("goc: target %s has no gc size model", target)
	}
	// Uintptr fixes the word size, and the widest scalars fix the maximum
	// alignment; together they are what every layout helper below depends on.
	for _, kind := range []types.BasicKind{types.Uintptr, types.Int, types.Int64, types.Float64, types.Complex128} {
		basic := types.Typ[kind]
		if actual.Sizeof(basic) == assumed.Sizeof(basic) && actual.Alignof(basic) == assumed.Alignof(basic) {
			continue
		}
		return fmt.Errorf(
			"goc: target %s lays %s out as %d/%d, not the %d/%d goc's type geometry assumes",
			target, basic, actual.Sizeof(basic), actual.Alignof(basic),
			assumed.Sizeof(basic), assumed.Alignof(basic),
		)
	}
	return nil
}
