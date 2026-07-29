// This program exercises ECDSA over all four NIST curves. The host Go toolchain
// prints eight `true` lines. Under goc it dies in
// crypto/internal/fips140/nistec.p224Table.Select with a fault at a small
// address, because three distinct closures share one symbol name.
//
// crypto/internal/fips140/nistec is generated code: p224.go, p384.go and
// p521.go are the same file with the curve name substituted, so their
// `<curve>GeneratorTableOnce.Do(func() {...})` literals all sit at line 393,
// column 28. goc names a function literal `<package>.func.<line>.<column>`
// whenever the enclosing function has no symbol of its own, which is every
// non-generic function (goc/reach.go sets functionDecl.symbol only for generic
// instantiations). All three literals are therefore called
// `crypto/internal/fips140/nistec.func.393.28`, and obj's ELF writer maps a name
// to one symbol-table index, so all seven relocations naming it resolve to
// whichever definition was written last. Two of the three generator tables are
// never built and stay nil.
//
// The same collision hits `<curve>BOnce.Do` at line 114, column 16.
//
// The capability matrix does not see this: stdlib_crypto_ecdsa.go uses only
// P-256, which takes the assembly path in p256_asm.go and never reaches the
// colliding closures.
//
// Recorded by the separate-compilation spike; see RUNTIME_PLAN.md 5.10.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

func exercise(name string, curve elliptic.Curve) {
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		fmt.Println(name, "generate error:", err)
		return
	}
	digest := sha256.Sum256([]byte("separate compilation spike"))
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		fmt.Println(name, "sign error:", err)
		return
	}
	fmt.Println(name, "verify:", ecdsa.VerifyASN1(&key.PublicKey, digest[:], signature))
	fmt.Println(name, "on curve:", curve.IsOnCurve(key.PublicKey.X, key.PublicKey.Y))
}

func main() {
	exercise("P224", elliptic.P224())
	exercise("P256", elliptic.P256())
	exercise("P384", elliptic.P384())
	exercise("P521", elliptic.P521())
}
