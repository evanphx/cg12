// Package memo is BUILD_CACHE.md's Option C: the whole-module optimiser stage
// memoised per function, keyed on the set of functions the inliner consulted to
// produce it, and validated on lookup.
//
// The shape of the thing, in one paragraph. A compile runs the front end, then
// the per-function prefix of the pipeline (`mem2reg` + the first `clean`), and
// stops. At that point every function has an *input digest* -- the bytes of its
// own post-prefix body -- and last compile left, for each function, a record of
// which other functions the inliner read on its behalf, what their input digests
// were, and what their recursion classification was. A function whose record
// still matches is a hit: its finished body is decoded from the memo and
// installed, the function is frozen so no pass in the stage may touch it, and
// the stage runs over what is left. A function whose record does not match is a
// miss and takes the full path.
//
// What makes that sound, and where it is not:
//
//   - Every pass in the stage but three is a [opt.FuncPass], a deterministic
//     function of the one function it is handed. Freezing one is invisible to
//     every other.
//   - The inliner reads other functions. It reads them through one choke point
//     (`directCallee`), which is where stage 1's recorder sits, so the recorded
//     set is every function whose body could have changed a decision about this
//     one -- including callees that were read and declined, which is the
//     conservative direction.
//   - `DeadFuncElim` is rerun rather than memoised. A function it drops is by
//     construction referenced by nothing live, so no surviving body depends on
//     the outcome; it costs 0.077 s on the small program.
//   - `InlineIntoNoSplitCallers` measures a frame the backend lays out and
//     charges a shared chain budget in call-graph order, which is a dependency on
//     an ordering and not on a set. Every `nosplit` function is therefore
//     excluded from the memo and takes the full path. It is a fixed population
//     (227 measured on a 5083-function module, 230 on a 14901-function one).
//   - SCC membership gates inlining absolutely and is a property of the whole
//     call graph, so it is in the key: each set member's `recursive` bit is
//     recorded and revalidated against a condensation rebuilt from the
//     post-prefix module.
//   - The residue that cannot be skipped -- one call graph + SCC build,
//     `DeadFuncElim`, `InlineIntoNoSplitCallers` -- is 0.467 s of a 16.16 s
//     compile on the small program and 1.164 s of 74.4 s on http.
//
// And the one place the model is an approximation rather than a proof, stated
// here rather than discovered later: the inliner walks the SCC condensation
// bottom-up and splices a callee's body *as it stands at that moment*, which for
// a callee that has not converged is not the body the stage finishes with. A
// frozen function is installed at its finished body from the start. So a missed
// function that reads a frozen one before the frozen one would have converged
// can see a body the reference compile would not have shown it. That is why
// there is a byte-identity guard over whole programs rather than an argument,
// and why [Options.RecomputeSpliceClosure] exists: it drags every function that
// was spliced into a missed function, transitively, back into the live set, which
// is the population stage 1 measured (31 invalidated, 125 dragged in, 156 of
// 4131).
package memo

import (
	"crypto/sha256"
	"fmt"
	"sort"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

// Digest identifies a function body, or a set of module-wide facts, by content.
type Digest [32]byte

func (d Digest) String() string { return fmt.Sprintf("%x", d[:8]) }

// Dep is one member of a function's recorded dependency set: a function the
// inliner resolved by name on its behalf, together with the two facts about that
// function a decision could have turned on.
type Dep struct {
	Name      string
	Input     Digest // the member's post-prefix body digest
	Recursive bool   // the member's SCC classification, over the post-prefix graph
}

// Entry is one function's memo of the whole-module stage.
type Entry struct {
	Name string
	// Input is the function's own post-prefix body digest.
	Input Digest
	// Recursive is the function's own SCC classification.
	Recursive bool
	// Module is the digest of the facts that belong to no single function:
	// ir.Module.SymAttrs, the CostInline selection, opt.PipelineIdentity, the
	// target, and the compiler binary.
	Module Digest
	// Deps is the recorded consulted set.
	Deps []Dep
	// Spliced names the functions whose bodies ended up inside this one. It is a
	// subset of Deps and it is not part of the key: it exists because a function
	// that has to be *recomputed* needs its splice sources to be live, so that
	// the inliner splices the mid-round body the reference compile spliced rather
	// than the finished one a frozen function stands at. Transitively closed, as
	// opt.InlineDeps closes it.
	Spliced []string
	// Body is the function marshalled in cg12's binary unit format, as the stage
	// finished with it -- or, for a function DeadFuncElim dropped, as it stood
	// when it was dropped, which is the last state anything could have read.
	Body []byte
	// Dropped records that the stage's DeadFuncElim removed this function from
	// the module. The body is kept anyway: a single-site callee is inlined and
	// then dropped, so a dropped function is exactly the kind another function
	// may have spliced.
	Dropped bool
	// Output is the finished body's digest, which is what the back-end memo is
	// keyed on. The back end is a pure function of the finished body, so that key
	// needs no dependency set at all.
	Output Digest
}

// Store is a set of memo entries, one per function name.
type Store struct {
	Entries map[string]*Entry
	// Data is the data-section digest the entries were written under. See
	// [DataDigest] for why it is here and not in every key.
	Data Digest
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{Entries: map[string]*Entry{}} }

// Lookup returns the entry recorded for a function name.
func (s *Store) Lookup(name string) *Entry { return s.Entries[name] }

// Put files an entry.
func (s *Store) Put(e *Entry) { s.Entries[e.Name] = e }

// Bytes reports the store's serialized size, which is what it costs on disk.
func (s *Store) Bytes() int64 {
	var total int64
	for _, e := range s.Entries {
		total += int64(len(e.Body)) + int64(len(e.Name)) + 32*3 + 4
		for _, d := range e.Deps {
			total += int64(len(d.Name)) + 32 + 1
		}
		for _, name := range e.Spliced {
			total += int64(len(name))
		}
	}
	return total
}

// FuncDigest is the input (and output) digest of one function: the sha256 of its
// body in cg12's binary unit format.
//
// The unit format rather than [ir.Func.String] because it is what the memo has to
// store anyway, because it is an order of magnitude cheaper to produce, and
// because it is the encoding [ir.CloneFunc] already round-trips through, so a
// body that digests equal decodes equal.
func FuncDigest(f *ir.Func) (Digest, []byte, error) {
	encoded, err := MarshalFunc(f)
	if err != nil {
		return Digest{}, nil, err
	}
	return sha256.Sum256(encoded), encoded, nil
}

// MarshalFunc encodes one function in cg12's binary unit format.
//
// It does *not* carry the owning module's file table, which ir.CloneFunc does.
// A SrcPos names a file by index, so what a decoded body needs is that the index
// still means the same file -- and it does, because the body goes back into the
// module it came out of (ir.Func.ReplaceBodyFrom keeps the owner). Carrying the
// table per function instead makes every entry pay for all of it: with the table
// in, the small program's memo is 81 MB and hashing every post-prefix body costs
// 0.23 s; with it out, 30 MB and 0.13 s. What the table has to be is *in the
// key*, so a compile whose file table is ordered differently does not use these
// bodies -- see ModuleDigest.
func MarshalFunc(f *ir.Func) ([]byte, error) {
	return (&ir.Module{Funcs: []*ir.Func{f}}).MarshalBinary()
}

// UnmarshalFunc decodes what [MarshalFunc] produced, checking it against the
// digest the entry recorded rather than against the IR verifier -- see
// [ir.DecodeModuleUnverified] for why that is the check this cache can use.
func UnmarshalFunc(data []byte, want Digest) (*ir.Func, error) {
	if got := Digest(sha256.Sum256(data)); got != want {
		return nil, fmt.Errorf("memo: unit digest %s, want %s", got, want)
	}
	decoded, err := ir.DecodeModuleUnverified(data)
	if err != nil {
		return nil, err
	}
	if len(decoded.Funcs) != 1 {
		return nil, fmt.Errorf("memo: encoded unit holds %d functions, want 1", len(decoded.Funcs))
	}
	return decoded.Funcs[0], nil
}

// ModuleDigest is the part of the key that belongs to no single function.
//
// SymAttrs is in it because it is the one piece of module state the per-function
// passes read (opt/pass.go names it: LoadElim and DeadAlloc, through
// isAtomicPointerStore) and the inliner reads it too, through
// isFrameScopedRuntimeCall. The front end writes it once and no pass touches it,
// so one digest covers it. The CostInline selection is in it because it is a
// whole-module decision made once on the original call graph. The rest is the
// identity of the compile itself.
func ModuleDigest(m *ir.Module, pipeline, target, compiler string) Digest {
	h := sha256.New()
	fmt.Fprintf(h, "pipeline\x00%s\x00target\x00%s\x00compiler\x00%s\x00", pipeline, target, compiler)
	// The file table, because a per-function unit stores positions as indices
	// into it and does not carry it (see MarshalFunc). Its order is a
	// deterministic function of what the front end read, so this is a clause that
	// changes when the answer would have changed and not otherwise.
	for i, name := range m.Files {
		fmt.Fprintf(h, "file\x00%d\x00%s\x00", i, name)
	}
	names := make([]string, 0, len(m.SymAttrs))
	for name := range m.SymAttrs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(h, "symattr\x00%s\x00%d\x00", name, m.SymAttrs[name])
	}
	for _, name := range opt.CostInlineSelected(m) {
		fmt.Fprintf(h, "costinline\x00%s\x00", name)
	}

	var out Digest
	copy(out[:], h.Sum(nil))
	return out
}

// DataDigest is the module's data section.
//
// It is deliberately *not* part of a per-function key. DeadFuncElim roots
// reachability at the globals, so which functions the stage drops depends on
// them -- but DeadFuncElim is rerun on every compile, from the actual module, so
// a per-function memo does not need to know. Putting it in the per-function key
// instead cost the whole cache on a one-character edit: changing 42 to 43 in the
// program moves a string literal, and 4607 of 4608 entries were invalidated by a
// clause none of them depended on.
//
// Where it *is* needed is the whole-module identity check, whose claim is that
// the stage's entire output is the stored output -- including which functions
// were dropped.
func DataDigest(m *ir.Module) Digest {
	encoded, err := (&ir.Module{Data: m.Data}).MarshalBinary()
	if err != nil {
		return Digest{}
	}
	return sha256.Sum256(encoded)
}

// Valid reports whether an entry may be used against the current compile, and
// says why not when it may not.
//
// This is the whole correctness argument in executable form, so it checks every
// clause rather than a single opaque hash of them: the function's own body, the
// module-wide facts, its own recursion classification, and for every member of
// its recorded set -- that the member is still in the module, that its body is
// unchanged, and that its recursion classification is unchanged.
func (e *Entry) Valid(own Digest, module Digest, recursive bool, inputs map[string]Digest, rec map[string]bool) (bool, string) {
	if e.Input != own {
		return false, "own input moved"
	}
	if e.Module != module {
		return false, "module facts moved"
	}
	if e.Recursive != recursive {
		return false, "own recursion classification moved"
	}
	for _, d := range e.Deps {
		got, ok := inputs[d.Name]
		if !ok {
			return false, "dependency " + d.Name + " left the module"
		}
		if got != d.Input {
			return false, "dependency " + d.Name + " moved"
		}
		if rec[d.Name] != d.Recursive {
			return false, "dependency " + d.Name + " changed recursion classification"
		}
	}
	return true, ""
}
