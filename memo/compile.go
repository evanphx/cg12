package memo

import (
	"fmt"
	"time"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

// Options configures one memoised run of the whole-module stage.
type Options struct {
	// Pipeline, Target and Compiler identify the compile; they go into the
	// module clause of every key.
	Pipeline string
	Target   string
	Compiler string

	// Disabled runs the stage exactly as an unmemoised compile does, ignoring
	// the store. It is the control arm: the same code path, the same split, the
	// same session, no freezing.
	Disabled bool

	// Closure decides which hits are demoted to the live set so that what a
	// recomputed function *reads* is what the reference compile would have shown
	// it. See [closureOf] for what each is and what it costs.
	//
	//	"none"   -- freeze every hit. Fastest, and exact only when there is
	//	            nothing live to read a frozen function at the wrong moment.
	//	"splice" -- drag in what a live function spliced, transitively.
	//	"read"   -- drag in everything a live function *read* that the stage
	//	            moves at all. This is the sound one.
	Closure string

	// Digests, when non-nil, is filled with every function's finished body
	// digest and whether it survived, so a memoised run and a control run can be
	// compared function by function rather than only object by object.
	Digests map[string]FinalState

	// Trace names functions whose digest is printed after every top-level pass
	// of the stage. It is how a byte-identity failure is attributed to a pass
	// instead of to the whole stage. Running the stage one pass at a time is the
	// same run: opt.Session.Run only iterates its slice.
	Trace []string
	// TraceTo receives the trace lines.
	TraceTo func(string)
	// TraceBody, when set, receives the whole body as well, so a divergence can
	// be read rather than inferred from a digest.
	TraceBody func(after string, f *ir.Func)
}

// FinalState is one function as the stage left it.
type FinalState struct {
	Digest   Digest
	Survives bool
}

func contains(funcs []*ir.Func, want *ir.Func) bool {
	for _, f := range funcs {
		if f == want {
			return true
		}
	}
	return false
}

// Result reports what one memoised stage did.
type Result struct {
	Funcs     int // functions handed to the stage
	Hits      int // memos used
	Misses    int // memos invalid or absent
	Excluded  int // nosplit functions, never memoised
	DraggedIn int // hits demoted to live by the live-set closure
	Static    int // entries whose body the stage does not move at all
	// WholeModule records that the stage was skipped outright because every
	// function matched -- the one case that is exact by construction.
	WholeModule bool
	Survivors   int // functions the stage left for the backend
	MissReasons map[string]int
	MissedNames []string

	DigestTime time.Duration // hashing every post-prefix body: the validation cost
	LookupTime time.Duration // validating keys and decoding the bodies of hits
	StageTime  time.Duration // the whole-module stage itself
	RecordTime time.Duration // marshalling the finished bodies back into the store
	StoreBytes int64

	// OutputOf is each function's finished body digest, which is the key the
	// back-end memo uses: the back end is a pure function of the finished body,
	// so that half needs no dependency set at all.
	OutputOf map[*ir.Func]Digest
}

// RunStage runs the whole-module stage over a module that has had the
// per-function prefix applied, using and refreshing the store.
//
// The caller owns the front end and the prefix, and must have run the prefix
// through the *same* [opt.Session] and the same `DefaultPipeline()` slice handed
// here -- for the reason cmd/splitprobe measures: rebuilding the pipeline across
// the split gives jump threading a second per-function budget and silently moves
// three functions.
func RunStage(m *ir.Module, session *opt.Session, stage []opt.Pass, store *Store, o Options) (*Result, error) {
	result := &Result{MissReasons: map[string]int{}}

	// Every function the stage is handed, including the ones it will drop: a
	// dropped function's body is still a body something spliced.
	all := append([]*ir.Func(nil), m.Funcs...)
	result.Funcs = len(all)

	if o.Disabled {
		// The control arm is a compile with no memo at all: no digesting, no
		// lookup, no recording. It has to be, or the baseline it sets would carry
		// the memo's overhead and flatter the saving.
		result.Misses = len(all)
		stageStart := time.Now()
		runStage(m, session, stage, o)
		result.StageTime = time.Since(stageStart)
		result.Survivors = len(m.Funcs)
		result.OutputOf = map[*ir.Func]Digest{}
		if o.Digests != nil {
			for _, f := range all {
				d, _, err := FuncDigest(f)
				if err != nil {
					return nil, err
				}
				o.Digests[f.Name] = FinalState{Digest: d, Survives: contains(m.Funcs, f)}
			}
		}
		return result, nil
	}

	// The validation cost stage 1 said to measure rather than assume: one walk of
	// the whole module, marshalling and hashing each body, plus one call graph
	// and SCC condensation for the recursion classification the key carries.
	digestStart := time.Now()
	inputs := make(map[string]Digest, len(all))
	inputOf := make(map[*ir.Func]Digest, len(all))
	for _, f := range all {
		d, _, err := FuncDigest(f)
		if err != nil {
			return nil, fmt.Errorf("memo: digest %s: %w", f.Name, err)
		}
		inputs[f.Name] = d
		inputOf[f] = d
	}
	recursive := make(map[string]bool)
	for f := range opt.RecursiveFunctions(m) {
		recursive[f.Name] = true
	}
	moduleDigest := ModuleDigest(m, o.Pipeline, o.Target, o.Compiler)
	dataDigest := DataDigest(m)
	result.DigestTime = time.Since(digestStart)

	lookupStart := time.Now()
	// The whole-module identity check, before the per-function one.
	//
	// If every function's post-prefix body digest matches its entry, the module
	// clause matches, and the module holds exactly the functions the store was
	// written from, then the input to the stage is bit-for-bit the input the
	// stored output came from. The stage is a deterministic function of that
	// input, so its output is the stored output -- for every function, including
	// the ones excluded from the per-function memo, and including whatever the
	// per-function key cannot express. That is a whole-module argument and it does
	// not depend on any of the per-function reasoning below being right.
	//
	// It is not a special case bolted on to make a benchmark look good; it is the
	// case a build cache is in most of the time (touch a file, change nothing,
	// build again) and it is the only case that is exact by construction.
	if hit, err := wholeModuleHit(all, m, store, inputOf, recursive, moduleDigest, dataDigest); err != nil {
		return nil, err
	} else if hit {
		result.Hits = len(all)
		result.WholeModule = true
		result.Survivors = len(m.Funcs)
		result.LookupTime = time.Since(lookupStart)
		outputs := make(map[*ir.Func]Digest, len(all))
		for _, f := range all {
			if e := store.Lookup(f.Name); e != nil {
				outputs[f] = e.Output
			}
		}
		result.OutputOf = outputs
		result.StoreBytes = store.Bytes()
		for _, e := range store.Entries {
			if e.Input == e.Output {
				result.Static++
			}
		}
		if o.Digests != nil {
			if err := fillDigests(all, m, o.Digests); err != nil {
				return nil, err
			}
		}
		return result, nil
	}

	frozen := map[*ir.Func]bool{}
	used := make(map[*ir.Func]*Entry, len(all))
	{
		for _, f := range all {
			// InlineIntoNoSplitCallers spends a shared nosplit chain budget in
			// call-graph order, so what one nosplit caller may do depends on what
			// an earlier one did. That is a dependency on an ordering, and no
			// per-function key expresses it: exclude the population outright.
			if f.NoSplit {
				result.Excluded++
				continue
			}
			entry := store.Lookup(f.Name)
			if entry == nil {
				result.Misses++
				result.MissReasons["no entry"]++
				continue
			}
			ok, why := entry.Valid(inputOf[f], moduleDigest, recursive[f.Name], inputs, recursive)
			if !ok {
				result.Misses++
				result.MissReasons[reasonClass(why)]++
				result.MissedNames = append(result.MissedNames, f.Name)
				continue
			}
			used[f] = entry
			frozen[f] = true
		}
		result.DraggedIn = closureOf(o.Closure, all, frozen, used, store)
		// Install the finished bodies only now, after the live set has settled:
		// dragInSpliceClosure can still demote a function.
		for f := range frozen {
			body, err := UnmarshalFunc(used[f].Body, used[f].Output)
			if err != nil {
				return nil, fmt.Errorf("memo: decode %s: %w", f.Name, err)
			}
			f.ReplaceBodyFrom(body)
		}
		result.Hits = len(frozen)
		session.Freeze(frozen)
	}
	result.LookupTime = time.Since(lookupStart)

	// The stage runs under the recorder, because the dependency set of every
	// function the stage recomputes has to be refiled with it.
	stageStart := time.Now()
	deps := opt.Record(func() { runStage(m, session, stage, o) })
	result.StageTime = time.Since(stageStart)
	result.Survivors = len(m.Funcs)

	recordStart := time.Now()
	if err := refresh(all, m, store, used, deps, inputs, inputOf, recursive, moduleDigest, result); err != nil {
		return nil, err
	}
	store.Data = dataDigest
	result.RecordTime = time.Since(recordStart)
	result.StoreBytes = store.Bytes()
	for _, e := range store.Entries {
		if e.Input == e.Output {
			result.Static++
		}
	}
	if o.Digests != nil {
		if err := fillDigests(all, m, o.Digests); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// runStage applies the stage, one pass at a time when a trace is asked for.
func runStage(m *ir.Module, session *opt.Session, stage []opt.Pass, o Options) {
	if len(o.Trace) == 0 || o.TraceTo == nil {
		session.Run(m, stage)
		return
	}
	watch := map[string]bool{}
	for _, name := range o.Trace {
		watch[name] = true
	}
	report := func(after string) {
		for _, f := range m.Funcs {
			if !watch[f.Name] {
				continue
			}
			d, _, err := FuncDigest(f)
			if err != nil {
				o.TraceTo(fmt.Sprintf("%-20s %-40s ERROR %v", after, f.Name, err))
				continue
			}
			o.TraceTo(fmt.Sprintf("%-20s %-52s %s", after, f.Name, d))
			if o.TraceBody != nil {
				o.TraceBody(after, f)
			}
		}
	}
	report("start")
	for i := range stage {
		session.Run(m, stage[i:i+1])
		report(fmt.Sprintf("%d.%s", i, stage[i].Name()))
	}
}

// wholeModuleHit checks that the store describes exactly this module, and if so
// installs every finished body and drops what the stage dropped.
func wholeModuleHit(all []*ir.Func, m *ir.Module, store *Store,
	inputOf map[*ir.Func]Digest, recursive map[string]bool, moduleDigest, dataDigest Digest) (bool, error) {

	if store.Data != dataDigest {
		return false, nil // the globals moved; DeadFuncElim may drop a different set
	}
	if len(store.Entries) != len(all) {
		return false, nil // a function appeared or left: not the same module
	}
	for _, f := range all {
		e := store.Lookup(f.Name)
		if e == nil || e.Input != inputOf[f] || e.Module != moduleDigest || e.Recursive != recursive[f.Name] {
			return false, nil
		}
	}
	survivors := make([]*ir.Func, 0, len(all))
	for _, f := range all {
		e := store.Lookup(f.Name)
		body, err := UnmarshalFunc(e.Body, e.Output)
		if err != nil {
			return false, fmt.Errorf("memo: decode %s: %w", f.Name, err)
		}
		f.ReplaceBodyFrom(body)
		if !e.Dropped {
			survivors = append(survivors, f)
		}
	}
	// The stage's DeadFuncElim is not rerun here: which functions it dropped is
	// recorded, and rerunning it over bodies that are already its own output
	// would drop exactly the same set.
	m.Funcs = survivors
	return true, nil
}

// refresh files an entry for every function the stage recomputed. A frozen
// function is left alone: its entry is the entry its body was installed from,
// and re-marshalling it would cost back most of what the hit saved.
func refresh(all []*ir.Func, m *ir.Module, store *Store, used map[*ir.Func]*Entry,
	deps *opt.InlineDeps, inputs map[string]Digest, inputOf map[*ir.Func]Digest,
	recursive map[string]bool, moduleDigest Digest, result *Result) error {

	survives := make(map[*ir.Func]bool, len(m.Funcs))
	for _, f := range m.Funcs {
		survives[f] = true
	}
	outputs := make(map[*ir.Func]Digest, len(all))
	for _, f := range all {
		if entry, ok := used[f]; ok {
			outputs[f] = entry.Output
			continue
		}
		output, encoded, err := FuncDigest(f)
		if err != nil {
			return fmt.Errorf("memo: encode %s: %w", f.Name, err)
		}
		outputs[f] = output
		entry := &Entry{
			Name:      f.Name,
			Input:     inputOf[f],
			Recursive: recursive[f.Name],
			Module:    moduleDigest,
			Body:      encoded,
			Dropped:   !survives[f],
			Output:    output,
		}
		for _, g := range consultedSet(f, deps, used, store) {
			entry.Deps = append(entry.Deps, Dep{
				Name:      g,
				Input:     inputs[g],
				Recursive: recursive[g],
			})
		}
		entry.Spliced = splicedSet(f, deps, used, store)
		store.Put(entry)
	}
	result.OutputOf = outputs
	return nil
}

// fillDigests records every function's finished state for the byte-identity
// comparison.
func fillDigests(all []*ir.Func, m *ir.Module, out map[string]FinalState) error {
	survives := make(map[*ir.Func]bool, len(m.Funcs))
	for _, f := range m.Funcs {
		survives[f] = true
	}
	for _, f := range all {
		d, _, err := FuncDigest(f)
		if err != nil {
			return err
		}
		out[f.Name] = FinalState{Digest: d, Survives: survives[f]}
	}
	return nil
}

// consultedSet is f's dependency set as this compile saw it, sorted.
//
// It is not simply what the recorder filed, and the difference is a soundness
// point. opt.InlineDeps closes a caller's set through its callees' sets when it
// splices one: a caller holding a clone of `mid` holds the clone of `leaf` that
// was already inside it, so it depends on `leaf`. A frozen callee was never
// processed in this compile, so the recorder has nothing to close through -- its
// transitive set lives in its stored entry. Union it in.
func consultedSet(f *ir.Func, deps *opt.InlineDeps, used map[*ir.Func]*Entry, store *Store) []string {
	set := map[string]bool{}
	if deps != nil {
		for _, g := range deps.Consulted(f) {
			set[g.Name] = true
		}
		for _, g := range deps.Spliced(f) {
			if _, frozen := used[g]; !frozen {
				continue
			}
			// A stored entry's Deps are already transitively closed, so one
			// union is enough.
			if entry := store.Lookup(g.Name); entry != nil {
				for _, d := range entry.Deps {
					set[d.Name] = true
				}
			}
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

// splicedSet is the set of functions whose bodies ended up inside f, closed the
// same way consultedSet is: a frozen callee contributes the closure its stored
// entry holds, because this compile never processed it and the recorder has
// nothing to close through.
func splicedSet(f *ir.Func, deps *opt.InlineDeps, used map[*ir.Func]*Entry, store *Store) []string {
	if deps == nil {
		return nil
	}
	set := map[string]bool{}
	for _, g := range deps.Spliced(f) {
		set[g.Name] = true
		if _, frozen := used[g]; !frozen {
			continue
		}
		if entry := store.Lookup(g.Name); entry != nil {
			for _, name := range entry.Spliced {
				set[name] = true
			}
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

// closureOf demotes hits to the live set until what every live function reads is
// what the reference compile would have shown it.
//
// The problem it solves, stated as the measurement found it. The stage does not
// hand a reader a function's finished body: the inliner walks the SCC
// condensation bottom-up and splices a callee *as it stands at that moment*, and
// both the inliner and the unroller size a callee with funcSize before deciding.
// A frozen function stands at its finished body from the start. So a live
// function that reads a frozen one before the frozen one would have converged can
// see something the reference compile would not have shown it -- measured, on the
// small program with *no misses at all*: the 475 live nosplit functions read
// frozen callees, runtime.unlockWithRank measured 24 instructions or fewer at its
// finished size where the reference measured more, and the unroller folded it
// into runtime.sysMemStat.add. One function of 5083, deterministically, and
// enough to change the object.
//
// "read" is the mode that closes it, and the closure is smaller than it sounds
// because of one fact: **1736 of 5083 functions on the small program have the same
// body after the stage as before it.** A function the stage never moves reads the
// same at every moment, so freezing it is exact by construction; only the ones the
// stage *does* move have to be live if a live function reads them.
//
// "splice" is the weaker reading stage 1's projection uses -- drag in what a live
// function spliced, and trust a size read. It is kept because the difference
// between the two is the price of exactness, and that price is worth a number.
func closureOf(mode string, all []*ir.Func, frozen map[*ir.Func]bool, used map[*ir.Func]*Entry, store *Store) int {
	switch mode {
	case "", "read":
		return dragInReadClosure(all, frozen, used, store)
	case "splice":
		return dragInSpliceClosure(all, frozen, used, store)
	case "none":
		return 0
	}
	panic("memo: unknown closure mode " + mode)
}

// dragInReadClosure demotes every function a live function reads and the stage
// moves, transitively.
func dragInReadClosure(all []*ir.Func, frozen map[*ir.Func]bool, used map[*ir.Func]*Entry, store *Store) int {
	byName := make(map[string]*ir.Func, len(all))
	for _, f := range all {
		byName[f.Name] = f
	}
	queue := make([]*ir.Func, 0, len(all))
	for _, f := range all {
		if !frozen[f] {
			queue = append(queue, f)
		}
	}
	dragged := 0
	for len(queue) > 0 {
		f := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		entry := store.Lookup(f.Name)
		if entry == nil {
			continue
		}
		for _, d := range entry.Deps {
			g := byName[d.Name]
			if g == nil || !frozen[g] {
				continue
			}
			read := store.Lookup(d.Name)
			if read != nil && read.Input == read.Output {
				continue // the stage never moves it; frozen reads the same
			}
			delete(frozen, g)
			delete(used, g)
			dragged++
			queue = append(queue, g)
		}
	}
	return dragged
}

// dragInSpliceClosure demotes to the live set every function whose body was
// spliced into a missed one, transitively.
//
// A missed function is recomputed, and recomputing it means the inliner splices
// a callee's body as it stands mid-round; a frozen callee stands at its finished
// body instead. Dragging the splice sources back in makes them live so they
// evolve as they would have. It is the reading stage 1's projection uses, and the
// cheap-to-build, expensive-to-run half of the choice it names.
//
// The seed is every function that is *not* frozen, which is the missed set plus
// the excluded nosplit population -- and the second half matters: with the seed
// taken from misses alone, a warm compile with no misses at all still emitted a
// different object, because the 475 live nosplit functions were splicing frozen
// callees. That is why an entry is filed for an excluded function too. It is
// never served as a hit; it is there so its splice sources can be found.
func dragInSpliceClosure(all []*ir.Func, frozen map[*ir.Func]bool, used map[*ir.Func]*Entry, store *Store) int {
	byName := make(map[string]*ir.Func, len(all))
	for _, f := range all {
		byName[f.Name] = f
	}
	queue := make([]*ir.Func, 0, len(all))
	for _, f := range all {
		if !frozen[f] {
			queue = append(queue, f)
		}
	}
	dragged := 0
	for len(queue) > 0 {
		f := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		entry := store.Lookup(f.Name)
		if entry == nil {
			continue
		}
		for _, name := range entry.Spliced {
			g := byName[name]
			if g == nil || !frozen[g] {
				continue
			}
			delete(frozen, g)
			delete(used, g)
			dragged++
			queue = append(queue, g)
		}
	}
	return dragged
}

// reasonClass buckets a validation failure for reporting without letting a
// function name into the bucket key.
func reasonClass(why string) string {
	switch why {
	case "own input moved", "module facts moved", "own recursion classification moved":
		return why
	}
	switch {
	case len(why) > 6 && why[len(why)-6:] == " moved":
		return "dependency moved"
	case len(why) > 20 && why[len(why)-20:] == "left the module":
		return "dependency left the module"
	}
	return "dependency changed"
}
