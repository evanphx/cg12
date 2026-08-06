package goc

import (
	"path/filepath"
	"strings"
	"sync"
)

// TrimPath is goc's equivalent of gc's -trimpath: it rewrites a source path the
// front end read into the form that gets interned into [ir.Module.Files], which
// is repository-relative and slash-separated.
//
// It exists because everything downstream of the front end is keyed, directly or
// indirectly, on the bytes of the IR, and until this existed those bytes named
// the build directory. goc.StdlibRoot is derived from runtime.Caller(0) and is
// therefore absolute, so every position in a module compiled from
// /home/x/checkout-a differed from the same position compiled from
// /home/x/checkout-b: measured, 3744 of 5083 per-function digests differed
// between two git worktrees of the same commit before any edit was made
// (CCWORK_REPORT.md, Option C stage 1). No cache -- the pack cache, Option B's
// per-package artifacts, or Option C's per-function memo -- can be shared
// between two checkouts, two machines or even two worktrees while that is true.
//
// It is unconditional rather than a flag. gc needs -trimpath to be optional
// because a debugger has to find the source, and gc emits DWARF that names it;
// goc emits no line table today, so the only consumers of the file table are
// [ir.Func.String], the escape diagnostics and the runtime-coverage mapping, all
// of which are inside this repository and are moved to the trimmed form with it.
// A switch here would be a knob that changes compiler output without changing
// the compiler binary, which is exactly the shape of the pack-cache hazard
// opt.PipelineIdentity exists to close.
//
// A path outside the repository is returned unchanged (only slash-normalized):
// the program being compiled is named by cmd/goc with filepath.Base already, and
// inventing a relative path for something above the root would be worse than
// leaving it alone.
func TrimPath(name string) string {
	if name == "" {
		return name
	}
	if !filepath.IsAbs(name) {
		return filepath.ToSlash(name)
	}
	rel, err := filepath.Rel(repositoryRoot(), name)
	if err != nil {
		return filepath.ToSlash(name)
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return filepath.ToSlash(name)
	}
	return rel
}

// UntrimPath is the inverse of [TrimPath] for the paths it rewrote: it resolves a
// repository-relative name back to an absolute one, so a consumer that has to
// open the file (the runtime-coverage mapping) still can. A path TrimPath left
// alone comes back unchanged.
func UntrimPath(name string) string {
	if name == "" || filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(repositoryRoot(), filepath.FromSlash(name))
}

// repositoryRoot is the goc checkout [StdlibRoot] lives in.
var repositoryRoot = sync.OnceValue(func() string {
	return filepath.Dir(repositoryStdlibRoot())
})
