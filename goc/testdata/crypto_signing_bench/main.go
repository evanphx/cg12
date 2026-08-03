// Command cryptosignbench measures elapsed time on the crypto signing path, one
// case per line, and is meant to be compiled and run by two compilers so the two
// answers can be put side by side:
//
//	go run ./cmd/goc -o /tmp/bench goc/testdata/crypto_signing_bench/main.go
//	go build -o /tmp/bench.gc goc/testdata/crypto_signing_bench/main.go
//
// goc/cryptobench_test.go does exactly that and joins the output against a
// committed baseline; see it for how the pair is turned into a regression gate.
//
// # Why this program exists
//
// This repository has instruments for how many allocations a program does
// (goc/testdata/alloc_census_baseline.txt, goc/testdata/slog_allocations_baseline.txt)
// and for where they land relative to the reference compiler
// (goc/testdata/escape_gc_differential.txt). It had none for how long anything
// takes. That gap is not hypothetical: the escape publication fix (6245dbb,
// "copying a value into fresh heap storage publishes it") was measured at +5.8%
// on 200 P-256 sign+verify when it landed, and because nothing watched elapsed
// time, that number went unverified for the seven compiler changes that landed
// after it. See CCWORK_REPORT.md, "Recovered: the bigmod.Nat.Mul cost of the
// escape publication fix".
//
// # Why the signing path in particular
//
// crypto/internal/fips140/bigmod is the arithmetic under ECDSA, and it is the
// worst case for allocation placement in the whole tree at once:
//
//   - Nat.Mul's default arm builds &Nat{limbs: T} from a locally made slice, so
//     it is decided by two different escape questions -- where T goes and where
//     the composite literal's address goes -- and gets a heap allocation if
//     either says heap. This is the exact shape 6245dbb changed.
//   - P-256's scalar field is four limbs, which takes that default arm; RSA's
//     1024/1536/2048-bit sizes take specialised arms instead. So the P-256 case
//     is sensitive to it and the RSA case is a control that is not.
//   - The whole path is aggregate-heavy value code with no assembly under goc,
//     so it is dominated by exactly what a code generator gets right or wrong.
//
// # Method
//
// Every case is a closure called through a func value, so neither compiler can
// see through the call and delete the work. Setup that is not being measured --
// key generation above all, which is itself expensive and highly variable
// because it rejection-samples -- happens once, outside the timer.
//
// measure runs the closure once to warm up (which faults in the code, fills the
// heap's size classes, and lets any lazily-initialised package tables settle),
// then runs measureRounds timed rounds and keeps the fastest. The minimum is the
// right estimator for elapsed time: scheduler preemption, GC, and any other
// machine noise can only ever make a round slower, never faster, so the fastest
// round is the one least contaminated by anything that is not the case under
// test. Reporting a mean instead would fold in whatever else the machine was
// doing, which is not a property of the compiler.
//
// The timer is time.Since on a monotonic reading, so it is unaffected by wall
// clock adjustment.
//
// control/empty-body and control/spin-fixed-work are in the table to say out
// loud that the instrument is being checked: the first must be ~0 (it measures
// the cost of the harness itself, and if it is not small then no other row means
// anything), and the second is a fixed amount of integer arithmetic that both
// compilers must be able to do, which gives the table a row that moves when the
// machine is loaded but not when escape analysis changes.
//
// # Reading the numbers
//
// Absolute nanoseconds are not comparable between machines and this program does
// not pretend otherwise. What is comparable is the ratio between two rows of the
// same run -- goc against gc on the same case, or p256/sign-verify against
// control/spin-fixed-work on the same compiler -- and that is what the baseline
// checks.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"fmt"
	"os"
	"sort"
	"time"
)

// measureRounds is how many timed rounds each case gets. Five is enough for the
// minimum to be stable in practice while keeping the whole program under a
// minute even under goc, which matters because the test compiles and runs it
// twice.
const measureRounds = 5

// signVerifyRounds is how many sign+verify round trips one timed round does. It
// is 200 because that is the size the original regression was measured at, so
// the recovered number and this program's number are the same quantity.
const signVerifyRounds = 200

func main() {
	cases := buildCases()

	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)

	if len(os.Args) > 1 && os.Args[1] == "-list" {
		for _, name := range names {
			fmt.Println(name)
		}
		return
	}

	fmt.Printf("# rounds=%d fastest-of signVerifyRounds=%d\n", measureRounds, signVerifyRounds)

	selected := names
	if len(os.Args) > 1 {
		selected = os.Args[1:]
	}
	for _, name := range selected {
		body, ok := cases[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "no such case: %s\n", name)
			os.Exit(2)
		}
		fmt.Printf("%s\t%d\n", name, int64(measure(body)))
	}
}

// measure returns the fastest of measureRounds timed rounds, in nanoseconds.
func measure(body func()) time.Duration {
	body()

	best := time.Duration(1<<63 - 1)
	for round := 0; round < measureRounds; round++ {
		start := time.Now()
		body()
		if elapsed := time.Since(start); elapsed < best {
			best = elapsed
		}
	}
	return best
}

// buildCases does every case's setup once and returns the timed bodies.
//
// Setup is deliberately outside the closures. ecdsa.GenerateKey and
// rsa.GenerateKey both rejection-sample against a random source, so their cost
// varies by a large factor from run to run; timing them would swamp the thing
// this program is for.
func buildCases() map[string]func() {
	cases := map[string]func(){}

	cases["control/empty-body"] = func() {}

	// A fixed amount of integer arithmetic, in a shape neither compiler can fold
	// away because the result is stored through a package-level sink.
	cases["control/spin-fixed-work"] = func() {
		accumulator := uint64(1)
		for i := 0; i < 20_000_000; i++ {
			accumulator = accumulator*6364136223846793005 + 1442695040888963407
		}
		sink = accumulator
	}

	p256Key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256([]byte("cg12 crypto signing benchmark"))

	// The headline case: the one the recovered regression was measured on.
	// bigmod.Nat.Mul's default arm is on this path and on no other case here.
	cases["p256/sign-verify"] = func() {
		for i := 0; i < signVerifyRounds; i++ {
			signature, err := ecdsa.SignASN1(rand.Reader, p256Key, digest[:])
			if err != nil {
				panic(err)
			}
			if !ecdsa.VerifyASN1(&p256Key.PublicKey, digest[:], signature) {
				panic("ecdsa signature did not verify")
			}
		}
	}

	// Verify alone. Signing draws from the random source and does a scalar
	// inversion; verification does neither, so splitting them says which half of
	// the round trip a movement is in.
	p256Signature, err := ecdsa.SignASN1(rand.Reader, p256Key, digest[:])
	if err != nil {
		panic(err)
	}
	cases["p256/verify"] = func() {
		for i := 0; i < signVerifyRounds; i++ {
			if !ecdsa.VerifyASN1(&p256Key.PublicKey, digest[:], p256Signature) {
				panic("ecdsa signature did not verify")
			}
		}
	}

	// P-384 uses six limbs, so it takes the same bigmod default arm as P-256 at
	// a different size. A change that is really about Nat.Mul moves both.
	p384Key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		panic(err)
	}
	cases["p384/sign-verify"] = func() {
		for i := 0; i < signVerifyRounds/4; i++ {
			signature, err := ecdsa.SignASN1(rand.Reader, p384Key, digest[:])
			if err != nil {
				panic(err)
			}
			if !ecdsa.VerifyASN1(&p384Key.PublicKey, digest[:], signature) {
				panic("ecdsa signature did not verify")
			}
		}
	}

	// The control on the crypto side. RSA-2048 goes through bigmod's specialised
	// 2048-bit arm, which does not build &Nat{limbs: T} the way the default arm
	// does, so a movement here is not a Nat.Mul movement.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	cases["rsa2048/sign-verify"] = func() {
		for i := 0; i < signVerifyRounds/40; i++ {
			signature, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, 0, digest[:])
			if err != nil {
				panic(err)
			}
			if err := rsa.VerifyPKCS1v15(&rsaKey.PublicKey, 0, digest[:], signature); err != nil {
				panic(err)
			}
		}
	}

	return cases
}

// sink keeps control/spin-fixed-work from being optimised away.
var sink uint64
