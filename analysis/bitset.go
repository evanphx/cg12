package analysis

import (
	"math/bits"

	"github.com/evanphx/cg12/ir"
)

// TempSet is a compact set of SSA temporaries, keyed by Temp.ID. It backs the
// dataflow analyses, where per-block sets are unioned and differenced often.
type TempSet struct {
	words []uint64
}

// NewTempSet returns a set sized to hold n temporaries.
func NewTempSet(n int) *TempSet {
	return &TempSet{words: make([]uint64, (n+63)/64)}
}

// Add inserts temp id and reports whether the set changed.
func (s *TempSet) Add(id int) bool {
	w, b := id/64, uint64(1)<<(uint(id)%64)
	if s.words[w]&b != 0 {
		return false
	}
	s.words[w] |= b
	return true
}

// AddRef inserts r if it names a temporary; other refs are ignored.
func (s *TempSet) AddRef(r ir.Ref) bool {
	if r.Kind == ir.RefTemp {
		return s.Add(int(r.ID))
	}
	return false
}

// Remove deletes temp id from the set.
func (s *TempSet) Remove(id int) {
	s.words[id/64] &^= uint64(1) << (uint(id) % 64)
}

// Has reports whether id is present.
func (s *TempSet) Has(id int) bool {
	return s.words[id/64]&(uint64(1)<<(uint(id)%64)) != 0
}

// Union adds every element of o, returning whether the set changed.
func (s *TempSet) Union(o *TempSet) bool {
	changed := false
	for i := range s.words {
		before := s.words[i]
		s.words[i] |= o.words[i]
		if s.words[i] != before {
			changed = true
		}
	}
	return changed
}

// Copy returns an independent copy of the set.
func (s *TempSet) Copy() *TempSet {
	w := make([]uint64, len(s.words))
	copy(w, s.words)
	return &TempSet{words: w}
}

// Len returns the number of elements.
func (s *TempSet) Len() int {
	n := 0
	for _, w := range s.words {
		n += bits.OnesCount64(w)
	}
	return n
}

// Members returns the element ids in ascending order.
func (s *TempSet) Members() []int {
	var out []int
	for wi, w := range s.words {
		for w != 0 {
			b := bits.TrailingZeros64(w)
			out = append(out, wi*64+b)
			w &= w - 1
		}
	}
	return out
}

// Equal reports whether two sets have identical membership.
func (s *TempSet) Equal(o *TempSet) bool {
	if len(s.words) != len(o.words) {
		return false
	}
	for i := range s.words {
		if s.words[i] != o.words[i] {
			return false
		}
	}
	return true
}
