package arm64

import "slices"

// regSet is a set of physical registers, one bit each.
//
// [Reg] ids run 0..vLast (X0..X30, SP, ZR, then V0..V31), which is 65 values and
// so does not fit one word. Two do, with room to spare. This replaced a
// map[Reg]bool per temporary, whose real cost was not its size but its reads:
// sizing the colour pool asks "is r forbidden for t" once per register per
// temporary, which on a -O compile of the SQLite amalgamation was about thirteen
// million map lookups where it is now thirteen million bit tests.
type regSet [2]uint64

func (s *regSet) add(r Reg) {
	s[uint(r)/64] |= 1 << (uint(r) % 64)
}

func (s *regSet) has(r Reg) bool {
	return s[uint(r)/64]&(1<<(uint(r)%64)) != 0
}

// interferenceGraph is the undirected graph of temporaries that are live at the
// same moment, held in compressed-sparse-row form: one flat array of
// neighbours, indexed by a per-node offset.
//
// It replaced a []map[int]bool -- one Go map per node, one entry per direction
// of every edge. That representation cost about 46 bytes for each of the 2.6
// million directed entries a -O compile of the SQLite amalgamation produces,
// some 150 MB, which was the largest single allocation site in the compiler
// outside the C preprocessor. The same graph here is one int32 per directed
// entry.
//
// The builder reports edges as it walks each block backwards and repeats
// itself: 1,711,099 reported pairs, of which 407,957 (23.8%) name a pair
// already reported. A map absorbed those silently. Here they are appended and
// removed once, in [interferenceGraph.seal], because the alternative -- keeping
// each row sorted as it grows -- pays a search on every one of the 1.7 million
// reports to save 24% of the space.
//
// Degrees must come out exactly right: the colourer removes a node when its
// degree falls below the number of colours available to it, so a neighbour
// counted twice would change which node spills and therefore the code emitted.
// That is what seal's deduplication is for, and why it happens before anything
// reads a row.
type interferenceGraph struct {
	nodes int
	// pairs holds (low<<32)|high while the graph is being built, duplicates and
	// all. It is released by seal.
	pairs []uint64
	// start indexes data; deg is how much of each row survived deduplication.
	// Rows keep the slack their duplicates occupied rather than being compacted,
	// which costs the 24% and saves a second pass over the whole array.
	start []int32
	deg   []int32
	data  []int32
}

func newInterferenceGraph(nodes int) *interferenceGraph {
	return &interferenceGraph{nodes: nodes}
}

// addPair records that a and b interfere. The caller guarantees a != b and that
// both are colourable nodes.
func (g *interferenceGraph) addPair(a, b int) {
	low, high := a, b
	if low > high {
		low, high = high, low
	}
	g.pairs = append(g.pairs, uint64(low)<<32|uint64(uint32(high)))
}

// seal organises the reported pairs into rows. Every row comes out ascending and
// free of duplicates. Nothing may be added afterwards, and nothing may be read
// before.
func (g *interferenceGraph) seal() {
	counts := make([]int32, g.nodes)
	for _, pair := range g.pairs {
		counts[int32(pair>>32)]++
		counts[int32(uint32(pair))]++
	}

	g.start = make([]int32, g.nodes+1)
	var total int32
	for node := 0; node < g.nodes; node++ {
		g.start[node] = total
		total += counts[node]
	}
	g.start[g.nodes] = total

	g.data = make([]int32, total)
	// filled doubles as the write cursor for each row and, once the fill is done,
	// as the row's length before deduplication.
	filled := make([]int32, g.nodes)
	for _, pair := range g.pairs {
		a, b := int32(pair>>32), int32(uint32(pair))
		g.data[g.start[a]+filled[a]] = b
		filled[a]++
		g.data[g.start[b]+filled[b]] = a
		filled[b]++
	}

	g.deg = make([]int32, g.nodes)
	for node := 0; node < g.nodes; node++ {
		row := g.data[g.start[node] : g.start[node]+filled[node]]
		slices.Sort(row)
		kept := 0
		for index, neighbour := range row {
			if index == 0 || neighbour != row[kept-1] {
				row[kept] = neighbour
				kept++
			}
		}
		g.deg[node] = int32(kept)
	}
	g.pairs = nil
}

// row returns node's neighbours, ascending. The slice aliases the graph and must
// not be modified.
func (g *interferenceGraph) row(node int) []int32 {
	return g.data[g.start[node] : g.start[node]+g.deg[node]]
}
