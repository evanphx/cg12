// Method dispatch on a value whose static type is a type parameter.
//
// This is the reduction of the crypto/ecdsa failure. The shape is the one
// crypto/internal/fips140/ecdsa uses for the NIST curves: a constraint whose
// type set is a union of pointer types, whose methods are declared in terms of
// the type parameter itself, and a carrier struct holding a `func() P`
// constructor. go/types reports the method selected on such a value as the
// constraint interface's method, which has no body; the call really targets the
// method of the type argument bound by the enclosing instantiation.
//
// Two instantiations share one generic source body, so a compiler that picks a
// single concrete method for the shared body gets one of them wrong.
package main

type affinePoint struct {
	x int
}

type jacobianPoint struct {
	x int
}

func newAffinePoint() *affinePoint {
	return &affinePoint{x: 1}
}

func newJacobianPoint() *jacobianPoint {
	return &jacobianPoint{x: 1}
}

func (point *affinePoint) ScalarBaseMult(scalar int) (*affinePoint, error) {
	return &affinePoint{x: point.x * scalar}, nil
}

func (point *affinePoint) Add(other *affinePoint) *affinePoint {
	return &affinePoint{x: point.x + other.x}
}

func (point *affinePoint) Coordinate() int {
	return point.x
}

func (point *jacobianPoint) ScalarBaseMult(scalar int) (*jacobianPoint, error) {
	return &jacobianPoint{x: point.x*scalar + 1000}, nil
}

func (point *jacobianPoint) Add(other *jacobianPoint) *jacobianPoint {
	return &jacobianPoint{x: point.x + other.x + 1000}
}

func (point *jacobianPoint) Coordinate() int {
	return point.x
}

type point[P any] interface {
	*affinePoint | *jacobianPoint

	ScalarBaseMult(int) (P, error)
	Add(P) P
	Coordinate() int
}

type curve[P point[P]] struct {
	newPoint func() P
}

// derive is the shared generic body. Every method call in it selects a
// constraint method on a type-parameter-typed value.
func derive[P point[P]](c *curve[P], scalar int) (int, error) {
	base, err := c.newPoint().ScalarBaseMult(scalar)
	if err != nil {
		return 0, err
	}
	doubled, err := base.ScalarBaseMult(2)
	if err != nil {
		return 0, err
	}
	return base.Add(doubled).Coordinate(), nil
}

func main() {
	affineCurve := &curve[*affinePoint]{newPoint: newAffinePoint}
	affineResult, err := derive(affineCurve, 7)
	if err != nil {
		panic(err)
	}
	// base.x == 7, doubled.x == 14, sum == 21.
	if affineResult != 21 {
		panic("affine point dispatch produced the wrong coordinate")
	}

	jacobianCurve := &curve[*jacobianPoint]{newPoint: newJacobianPoint}
	jacobianResult, err := derive(jacobianCurve, 7)
	if err != nil {
		panic(err)
	}
	// base.x == 1007, doubled.x == 3014, sum == 5021.
	if jacobianResult != 5021 {
		panic("jacobian point dispatch produced the wrong coordinate")
	}
}
