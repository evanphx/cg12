// Background mark workers, in all three modes the pacer can hand out.
//
// gcControllerState.startCycle sets dedicatedMarkWorkersNeeded to
// GOMAXPROCS*0.25 rounded down and fractionalUtilizationGoal to the remainder,
// so GOMAXPROCS=4 asks for exactly one dedicated worker and no fractional one,
// while GOMAXPROCS=3 asks for three quarters of a worker and gets a fractional
// one instead. Idle workers appear whenever a P would otherwise go to sleep
// during a mark phase. Each mode drains the same work queue through its own
// wrapper, and a mode that never runs means the pacer never handed a P to it.
//
// A program cannot tell the modes apart: cpuStats.accumulate folds fractional
// time into GCDedicatedTime, so /cpu/classes/gc/mark/dedicated:cpu-seconds
// covers both. What this program is responsible for is producing enough mark
// work, over a long enough mark phase, that the pacer has a reason to start
// workers at all -- and then checking that everything the workers marked is
// still intact. Which modes actually ran is asserted separately, by
// TestMarkWorkerModesAreAllReached, against the runtime coverage bitmap.
//
// The live set is a pointer-dense graph rather than a flat slice, because a
// worker is only interesting if it has to trace: a heap of pointer-free bytes
// gives the mark phase nothing to do.
package main

import (
	"runtime"
	"runtime/metrics"
	"sync"
)

type vertex struct {
	value int
	label string
	edges []*vertex
}

const (
	graphWidth = 512
	graphDepth = 6
	workers    = 8
)

//go:noinline
func buildGraph(seed int) *vertex {
	root := &vertex{value: seed, label: "root"}
	frontier := []*vertex{root}
	for depth := 0; depth < graphDepth; depth++ {
		var next []*vertex
		for _, parent := range frontier {
			for child := 0; child < 3; child++ {
				node := &vertex{
					value: parent.value*3 + child,
					label: "node-" + string(rune('a'+(depth+child)%26)),
				}
				parent.edges = append(parent.edges, node)
				next = append(next, node)
			}
		}
		frontier = next
		if len(frontier) > graphWidth {
			frontier = frontier[:graphWidth]
		}
	}
	return root
}

//go:noinline
func sumGraph(root *vertex) int {
	if root == nil {
		panic("the mark phase lost a graph root")
	}
	total := root.value
	if root.label == "" {
		panic("the mark phase lost a vertex label")
	}
	for _, edge := range root.edges {
		total += sumGraph(edge)
	}
	return total
}

// churn keeps allocating and dropping pointer-dense garbage, which is what makes
// a mark phase long enough for the pacer to want workers on it.
//
//go:noinline
func churn(rounds int) {
	for round := 0; round < rounds; round++ {
		local := buildGraph(round)
		if local.value != round {
			panic("churn built the wrong graph")
		}
	}
}

//go:noinline
func readMarkCPU() (dedicated, idle, assist float64) {
	samples := []metrics.Sample{
		{Name: "/cpu/classes/gc/mark/dedicated:cpu-seconds"},
		{Name: "/cpu/classes/gc/mark/idle:cpu-seconds"},
		{Name: "/cpu/classes/gc/mark/assist:cpu-seconds"},
	}
	metrics.Read(samples)
	for index, sample := range samples {
		if sample.Value.Kind() != metrics.KindFloat64 {
			println("metric", index, "has kind", int(sample.Value.Kind()))
			panic("a GC CPU metric is not a float64")
		}
	}
	return samples[0].Value.Float64(), samples[1].Value.Float64(), samples[2].Value.Float64()
}

func main() {
	// A live graph that every cycle has to trace in full.
	retained := make([]*vertex, 0, 16)
	for index := 0; index < 16; index++ {
		retained = append(retained, buildGraph(index))
	}
	expected := make([]int, len(retained))
	for index, root := range retained {
		expected[index] = sumGraph(root)
	}

	var running sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		running.Add(1)
		go func(seed int) {
			defer running.Done()
			churn(24)
		}(worker)
	}

	for cycle := 0; cycle < 6; cycle++ {
		runtime.GC()
	}
	running.Wait()
	runtime.GC()

	for index, root := range retained {
		if got := sumGraph(root); got != expected[index] {
			println("graph", index, "sums to", got, "want", expected[index])
			panic("a retained graph changed across concurrent marking")
		}
	}

	dedicated, idle, assist := readMarkCPU()
	if dedicated <= 0 && idle <= 0 {
		println("dedicated", int(dedicated*1e9), "idle", int(idle*1e9), "ns")
		panic("no background mark worker CPU was accounted at all")
	}
	// Assist time is not required here: with six forced collections and eight
	// churning goroutines the pacer may keep up without charging anyone.
	_ = assist

	println("mark workers ok")
}
