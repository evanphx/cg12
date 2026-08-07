//go:build ignore

// probe_pointer_generics.go is the best case for GC shape stenciling that a Go
// program can be: several program-local pointer types, each pushed through the
// same stdlib generics and the same program-declared generics. Nothing in
// goc/testdata does this -- fmt_sprintf.go declares no types at all and
// stdlib_http_tls_client_server.go declares no named types -- so without a
// probe the census would report "no instantiation uses an importing-program
// type" and leave it ambiguous whether that is a fact about Go programs or a
// fact about two corpus programs.
//
// It is deliberately the same program the shape model was validated against
// under the real gc (`go build` + `go tool nm`), so the census's answer and
// gc's symbol table can be compared instantiation by instantiation.
//
// Not in goc/testdata, because it is a measurement input and not a corpus case.
// Census it with:
//
//	go test ./goc/ -run TestGenericShapeCensus -generic-shape-census=<dir> \
//	  -generic-shape-census-program=../analysis/genericshape/probe_pointer_generics.go
package main

import (
	"slices"
	"sync"
	"sync/atomic"
)

type alpha struct{ x int }
type beta struct{ y int }
type gamma struct{ z string }
type delta struct{ p *alpha }

// pick is a program-declared generic over a basic constraint: the case that
// collapses hardest under shapes.
func pick[T any](values []T, index int) T {
	var accumulator [4]T
	for i := range accumulator {
		accumulator[i] = values[index]
	}
	return accumulator[len(accumulator)-1]
}

func main() {
	a := []*alpha{{3}, {1}, {2}}
	b := []*beta{{3}, {1}, {2}}
	c := []*gamma{{"ccc"}, {"a"}, {"bb"}}
	d := []*delta{{nil}, {&alpha{1}}, {nil}}

	slices.SortFunc(a, func(p, q *alpha) int { return p.x - q.x })
	slices.SortFunc(b, func(p, q *beta) int { return p.y - q.y })
	slices.SortFunc(c, func(p, q *gamma) int { return len(p.z) - len(q.z) })
	slices.SortFunc(d, func(p, q *delta) int { return len(p.z()) - len(q.z()) })

	println(a[0].x, b[0].y, c[0].z, d[0].p == nil)
	println(slices.IndexFunc(a, func(p *alpha) bool { return p.x == 1 }))
	println(slices.IndexFunc(b, func(p *beta) bool { return p.y == 1 }))
	println(slices.IndexFunc(c, func(p *gamma) bool { return p.z == "a" }))
	println(slices.IndexFunc(d, func(p *delta) bool { return p.p != nil }))
	println(len(slices.Clone(a)), len(slices.Clone(b)), len(slices.Clone(c)), len(slices.Clone(d)))

	println(pick(a, 0).x, pick(b, 0).y, pick(c, 0).z, pick(d, 0).p == nil)

	// The same generics over the program's *value* types, which under gc's rule
	// keep their layout and so mostly do not collapse.
	va := []alpha{{2}, {1}}
	vb := []beta{{2}, {1}}
	vc := []gamma{{"bb"}, {"a"}}
	slices.SortFunc(va, func(p, q alpha) int { return p.x - q.x })
	slices.SortFunc(vb, func(p, q beta) int { return p.y - q.y })
	slices.SortFunc(vc, func(p, q gamma) int { return len(p.z) - len(q.z) })
	println(va[0].x, vb[0].y, vc[0].z)
	println(pick(va, 0).x, pick(vb, 0).y, pick(vc, 0).z)

	// atomic.Pointer[T] takes the pointee, not the pointer, so it is the case
	// gc's shallow rule cannot collapse.
	var pa atomic.Pointer[alpha]
	var pb atomic.Pointer[beta]
	var pc atomic.Pointer[gamma]
	pa.Store(&alpha{1})
	pb.Store(&beta{2})
	pc.Store(&gamma{"3"})
	println(pa.Load().x, pb.Load().y, pc.Load().z)

	// sync.OnceValue[T] over program pointer types is the pure pointer case.
	oa := sync.OnceValue(func() *alpha { return &alpha{7} })
	ob := sync.OnceValue(func() *beta { return &beta{8} })
	oc := sync.OnceValue(func() *gamma { return &gamma{"9"} })
	println(oa().x, ob().y, oc().z)
}

func (d *delta) z() string {
	if d.p == nil {
		return ""
	}
	return "p"
}
