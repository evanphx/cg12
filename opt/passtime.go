package opt

import (
	"os"
	"runtime/metrics"
	"sort"
	"sync"
	"time"
)

// Per-pass optimiser accounting, off unless GOC_OPT_PASSTIME=1.
//
// [FuncTiming] answers "which function costs the most"; this answers the other
// question, "which pass costs the most, and how much of that is re-proving a
// fixpoint it had already reached". The two together say where a compile's time
// goes: one names the input, the other names the code doing the work.
//
// WHY LEAF PASSES ONLY. Every pass invocation flows through [runPass], which
// makes it the one hook that sees them all -- but a [Fixpoint] is itself a Pass
// and calls runPass again for each of its members, so timing every runPass call
// would count a fixpoint's members once inside their own row and again inside
// the fixpoint's. So the leaves are timed and the fixpoints are only counted:
// enters and rounds. The leaf rows then sum to the optimiser's total, which is
// the property that makes the table readable.
//
// WHAT IS TRUSTWORTHY. Visits, skips and changes are exact counts. Allocation is
// read from runtime/metrics rather than runtime.ReadMemStats: the counters are
// cumulative and monotonic, so a delta across a pass is exactly what that pass
// allocated, and reading them does not stop the world the way ReadMemStats does
// (this hook runs hundreds of times per compile). Live heap is deliberately NOT
// reported per pass: without a forced GC the number says more about when the
// collector last ran than about the pass, and forcing one per pass would distort
// the times. Measure live heap at stage boundaries instead -- cmd/ccstat does.
//
// WHAT IS NOT. Allocation attribution assumes one compile at a time. cmd/cc and
// cmd/ccstat are serial, so it holds there. goc compiles packages concurrently
// (goc/optcache.go), and under that driver another package's allocations land in
// whichever pass happens to be running, so read only the counts, not the bytes.
//
// When the switch is off the cost is one bool test per pass invocation and one
// per function visit.

var passTimingEnabled = os.Getenv("GOC_OPT_PASSTIME") == "1"

// passKey identifies one position in a pipeline. Two [FuncPass]es can share a
// name -- the default pipeline has three "dce"s -- so the id assigned at
// construction separates them. The leaves that carry no id ([ModulePass], the
// inliner) are unique by name in every pipeline in the tree, and key on 0.
type passKey struct {
	name string
	id   int32
}

// PassStat is what one position in the pipeline cost.
type PassStat struct {
	Name string
	// ID distinguishes two positions sharing a name; 0 for a pass with no
	// per-position identity. See [passKey].
	ID int32
	// Invocations counts entries into the pass, Visits the functions it actually
	// ran on, and Skipped the functions it declined to revisit because the change
	// log already had them converged. Skipped is the waste the log removes, and
	// Changed against Visits is how much of the remaining work pays.
	Invocations int64
	Visits      int64
	Skipped     int64
	Changed     int64
	Nanos       int64
	// Bytes and Objects are allocated, not retained. See the note above about
	// concurrent compiles.
	Bytes   uint64
	Objects uint64
}

// FixpointStat is how hard one fixpoint had to iterate. Rounds counts every
// traversal of the member list, including the last one that proved convergence
// by changing nothing -- that final round is pure overhead by construction, so
// Rounds minus Enters is the number of rounds that did some work.
type FixpointStat struct {
	Name   string
	Enters int64
	Rounds int64
}

var passTiming struct {
	sync.Mutex
	stats     map[passKey]*PassStat
	order     []passKey
	fixpoints map[string]*FixpointStat
	fixOrder  []string
	sample    []metrics.Sample
}

const (
	allocBytesMetric   = "/gc/heap/allocs:bytes"
	allocObjectsMetric = "/gc/heap/allocs:objects"
)

// allocatedLocked reads the cumulative allocation counters. The caller holds the
// lock, which is also what makes the shared sample slice safe.
func allocatedLocked() (bytes, objects uint64) {
	if passTiming.sample == nil {
		passTiming.sample = []metrics.Sample{
			{Name: allocBytesMetric},
			{Name: allocObjectsMetric},
		}
	}
	metrics.Read(passTiming.sample)
	return passTiming.sample[0].Value.Uint64(), passTiming.sample[1].Value.Uint64()
}

// allocated is allocatedLocked for a caller that does not already hold the lock.
func allocated() (bytes, objects uint64) {
	passTiming.Lock()
	defer passTiming.Unlock()
	return allocatedLocked()
}

// statLocked returns the record for a key, creating it in first-seen order so
// the report can be printed in pipeline order when time order is not wanted.
func statLocked(key passKey) *PassStat {
	if passTiming.stats == nil {
		passTiming.stats = make(map[passKey]*PassStat)
	}
	stat, ok := passTiming.stats[key]
	if !ok {
		stat = &PassStat{Name: key.name, ID: key.id}
		passTiming.stats[key] = stat
		passTiming.order = append(passTiming.order, key)
	}
	return stat
}

// recordPassRun files one leaf pass invocation.
func recordPassRun(key passKey, elapsed time.Duration, bytes, objects uint64) {
	passTiming.Lock()
	defer passTiming.Unlock()
	stat := statLocked(key)
	stat.Invocations++
	stat.Nanos += int64(elapsed)
	stat.Bytes += bytes
	stat.Objects += objects
}

// recordPassVisits files one per-function pass invocation's worth of visits.
//
// It takes the whole invocation's counts rather than one visit at a time because
// a pass sees a quarter of a million functions in a whole-module compile, and a
// lock per function would measure the accounting instead of the pass. The caller
// counts in locals and hands the totals over once.
func recordPassVisits(key passKey, visits, skipped, changed int64) {
	passTiming.Lock()
	defer passTiming.Unlock()
	stat := statLocked(key)
	stat.Visits += visits
	stat.Skipped += skipped
	stat.Changed += changed
}

// passKeyOf identifies the pipeline position a pass occupies. Only [FuncPass]
// carries a per-position id; the other leaves are unique by name.
func passKeyOf(p Pass) passKey {
	if leaf, ok := p.(funcPass); ok {
		return passKey{name: leaf.name, id: leaf.id}
	}
	return passKey{name: p.Name()}
}

// recordFixpointRound files one traversal of a fixpoint's member list; enter
// records the entry into the fixpoint itself.
func recordFixpointRound(name string, enter bool) {
	passTiming.Lock()
	defer passTiming.Unlock()
	if passTiming.fixpoints == nil {
		passTiming.fixpoints = make(map[string]*FixpointStat)
	}
	stat, ok := passTiming.fixpoints[name]
	if !ok {
		stat = &FixpointStat{Name: name}
		passTiming.fixpoints[name] = stat
		passTiming.fixOrder = append(passTiming.fixOrder, name)
	}
	if enter {
		stat.Enters++
		return
	}
	stat.Rounds++
}

// PassTiming reports what each position in the pipeline cost, in descending time
// order, or nil when GOC_OPT_PASSTIME was not set.
func PassTiming() []PassStat {
	passTiming.Lock()
	defer passTiming.Unlock()
	if len(passTiming.stats) == 0 {
		return nil
	}
	stats := make([]PassStat, 0, len(passTiming.order))
	for _, key := range passTiming.order {
		stats = append(stats, *passTiming.stats[key])
	}
	sort.SliceStable(stats, func(i, j int) bool {
		if stats[i].Nanos != stats[j].Nanos {
			return stats[i].Nanos > stats[j].Nanos
		}
		return stats[i].Name < stats[j].Name
	})
	return stats
}

// FixpointTiming reports how hard each fixpoint iterated, in pipeline order, or
// nil when GOC_OPT_PASSTIME was not set.
func FixpointTiming() []FixpointStat {
	passTiming.Lock()
	defer passTiming.Unlock()
	if len(passTiming.fixpoints) == 0 {
		return nil
	}
	stats := make([]FixpointStat, 0, len(passTiming.fixOrder))
	for _, name := range passTiming.fixOrder {
		stats = append(stats, *passTiming.fixpoints[name])
	}
	return stats
}

// ResetPassTiming discards everything recorded so far. It is for a harness that
// measures several compiles in one process and wants them apart.
func ResetPassTiming() {
	passTiming.Lock()
	defer passTiming.Unlock()
	passTiming.stats = nil
	passTiming.order = nil
	passTiming.fixpoints = nil
	passTiming.fixOrder = nil
}

// PassTimingEnabled reports whether GOC_OPT_PASSTIME asked for this accounting,
// so a harness can say plainly that it has nothing to show rather than printing
// an empty table.
func PassTimingEnabled() bool { return passTimingEnabled }
