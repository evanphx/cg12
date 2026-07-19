# cg12 standard-library overlays

This directory contains temporary, explicit differences from the unchanged
standard library under `stdlib/src`.

Overlay implementations must be conventional Go (`.go`) or cg12's native
textual IR (`.ssa`). Plan 9 and GNU assembly are deliberately not accepted as
overlay formats. The upstream assembly remains in `stdlib/src` as the source
against which semantic lowering is tested.

Native `.ssa` overlays may contain cg12 inline-assembly instructions. Inline
assembly is parsed into `OAsm`, carries explicit operands and clobbers through
the optimizer and register allocator, and is assembled by cg12's target backend.
It is not a sidecar GNU assembly file. Architecture-specific entries must also
declare `goarch` (and normally `goos`) constraints in the manifest.

`manifest.json` records the target, reason, upstream hash, and removal condition
for every replacement or addition. Replacements fail closed when the copied
upstream file changes. Set `GOC_STDLIB_OVERLAY=off` to compile the untouched
tree, or set it to another manifest path to use a different overlay set.

The initial overlay is intentionally generous during runtime bring-up. Once the
runtime and foreign-entry tests pass, entries should be removed one at a time
and the same tests rerun against the exact upstream sources.
