# Two goc miscompiles found while measuring log/slog's allocations

These are reductions written while building
`goc/testdata/slog_allocations`. They are not about allocation: they are
programs the host Go toolchain runs correctly and goc does not, found because
the allocation benchmark was the first thing in this tree to call `log/slog` in
a loop with a real handler.

Each bug is here twice, as the failing program and as the nearest control that
works, because the control is what says what the bug is about.

They are deliberately **not** in `goc/testdata/`, so `filepath.Glob("testdata/*.go")`
does not pick them up and they do not change the corpus, the allocation census
or any baseline. Run one with

    go run ./cmd/goc -run goc/testdata/slog_allocations/miscompiles/<name>.go

and compare against

    go run goc/testdata/slog_allocations/miscompiles/<name>.go

Recorded against **go1.26.1 linux/arm64** and goc at `main` `a535466`. Every
program here prints its answer under gc; a future goc run that prints the same
answer is the fix, and a future run that prints something else again is a new
finding.

## attr_bad_pointer.go — a `slog.Attr` in a frame across a collection

    fatal error: invalid pointer found on stack
    runtime: bad pointer in frame main_main at 0x...: 0xc8

Thirteen lines. `slog.Int("k", 200)` is passed to a `//go:noinline` function
that calls `runtime.GC()` before touching it. `0xc8` is 200 -- the integer the
attribute carries in `Value.num`, which `log/slog` packs into a `uint64` field
precisely so that it is *not* a pointer. Something in the frame's pointer map
says that word is one, and the collector rejects it.

The bad word is in `main_main`'s frame, not the callee's: it is the caller's
copy of the returned `Attr`, so the miscompile is at the value's origin rather
than at the call.

gc prints `200`. This is why `json/kv-4-pairs` and `json/logattrs-4-attrs` are
`crash` in `../../slog_allocations_baseline.txt`: the JSON handler allocates
enough, and recurses deep enough, to hit a collection or a stack copy with an
attribute live in a frame, which nothing else in the table does.

**Severity.** Any goroutine holding a `slog.Attr` in a frame when a collection
happens can die. It is not a JSON-handler bug and it is not a load-dependent
race; the reduction here forces the collection and fails 5 runs out of 5, at the
same offset, with the same value.

### attr_bad_pointer_stackcopy.go — the same bug through the other path

The same shape with the collection replaced by a recursion deep enough to copy
the stack. `runtime.adjustpointers` rejects the same word. Both halves of the
runtime that walk a frame -- the collector and the stack copier -- disagree with
the pointer map, so this is the map being wrong and not one walker being wrong.

### attr_bad_pointer_control.go — the control, which works

`slog.Value`'s exact shape, hand-written in the program's own package: a
`_ [0]func()` field, a `uint64`, an `any`, wrapped in a struct with a leading
`string` key, returned by value from a `//go:noinline` constructor, held across
a `runtime.GC()`. It prints `200` under both compilers.

So the zero-length function field is not on its own enough to produce the bad
map, and neither is returning the struct by value from a non-inlined function.
Whatever the trigger is, this control does not have it, which is the most useful
thing it can say.

## pkginit_dispatch.go — an interface built in a package-level initializer

    cg12: interface dispatch failed for dynamic type 0x...
    fatal error: cg12: interface dispatch failure
      log_slog_Handler_Enabled
      log_slog_Logger_Enabled

Nine lines:

    var jsonLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))

The program links, and the first call through `slog.Handler` fails to dispatch:
goc's table has no entry for the handler's dynamic type. The conversion from
`*slog.JSONHandler` to `slog.Handler` happens as the argument to `slog.New`, in
a package-level variable initializer, and nothing else in the program converts
that type -- so if that site is not what registers it, nothing does.

gc prints `json ok`.

### pkginit_dispatch_control.go — the control, which works

The identical expression, assigned to the identical package-level variable, from
inside `main` instead of from the initializer. It prints `json ok` under both
compilers, which is what makes the initializer the subject rather than
`slog.New`, `io.Discard`, or the JSON handler.

Three more controls, run but not kept, narrow it further:

  * `var h slog.Handler = slog.NewJSONHandler(io.Discard, nil)` -- the same
    conversion straight into a variable whose declared type is the interface --
    works. It is the conversion at a **call argument** that is missed.
  * The same shape with a type and a function defined in the program's own
    package (`var b = newBox(&dog{n: 7})`) works, so a main-package type reaches
    the table some other way.
  * A main-package type passed to a stdlib function in an initializer
    (`var r = bufio.NewReader(myReader{})`) works, and a stdlib type passed to a
    main-package function in an initializer
    (`var w = wrap(slog.NewJSONHandler(io.Discard, nil))`) **fails**. So the
    missed registration follows the converted type, not the callee.

`../main.go` routes around this by building its loggers in a function. That is a
workaround in a benchmark, not a fix, and it is worth noticing that the workaround
was necessary: the natural way to write a package-level logger is the way that
does not work.
