package opt

import (
	"os"
	"sort"
	"sync"
	"time"
)

// Per-function optimiser accounting, off unless GOC_OPT_FUNCTIME=1.
//
// It exists to answer "what share of the optimiser's time goes into generic
// instantiations". Almost every pass enters the pipeline through [FuncPass],
// which is a single choke point around one transform applied to one function,
// so timing there attributes nearly all of the pipeline's work to a named
// function. What it cannot attribute is the interprocedural passes ([Inline],
// [DeadFuncElim]): those are [ModulePass]es and their time is reported
// separately as unattributed, so a reader can see how much of the total the
// per-function figure is a share of.
//
// When the switch is off the cost is one bool test per function per pass. When
// it is on the compile is slower -- a time.Now pair per visit, and there are
// hundreds of thousands of visits -- which does not matter, because the number
// wanted is a ratio between functions measured in the same run.

var funcTimingEnabled = os.Getenv("GOC_OPT_FUNCTIME") == "1"

var funcTiming struct {
	sync.Mutex
	nanos  map[string]int64
	visits map[string]int64
}

func recordFuncTime(name string, elapsed time.Duration) {
	funcTiming.Lock()
	if funcTiming.nanos == nil {
		funcTiming.nanos = make(map[string]int64)
		funcTiming.visits = make(map[string]int64)
	}
	funcTiming.nanos[name] += int64(elapsed)
	funcTiming.visits[name]++
	funcTiming.Unlock()
}

// FuncTiming reports the accumulated per-function optimiser time in
// nanoseconds and the number of pass visits each function took, or nil when
// GOC_OPT_FUNCTIME was not set.
func FuncTiming() (nanos map[string]int64, visits map[string]int64) {
	funcTiming.Lock()
	defer funcTiming.Unlock()
	if funcTiming.nanos == nil {
		return nil, nil
	}
	nanos = make(map[string]int64, len(funcTiming.nanos))
	visits = make(map[string]int64, len(funcTiming.visits))
	for name, value := range funcTiming.nanos {
		nanos[name] = value
	}
	for name, value := range funcTiming.visits {
		visits[name] = value
	}
	return nanos, visits
}

// FuncTimingNames returns the timed functions in descending time order. It is a
// convenience for reporting; the maps from [FuncTiming] are the data.
func FuncTimingNames() []string {
	nanos, _ := FuncTiming()
	names := make([]string, 0, len(nanos))
	for name := range nanos {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if nanos[names[i]] != nanos[names[j]] {
			return nanos[names[i]] > nanos[names[j]]
		}
		return names[i] < names[j]
	})
	return names
}
