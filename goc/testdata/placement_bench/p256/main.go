// Command p256bench times ECDSA P-256 sign+verify, which is bigmod limb
// arithmetic: the workload the crypto signing benchmark's headline row measures,
// trimmed to one case so a placement sweep can build and run it many times.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"time"
)

// rounds is how many timed rounds each case gets; the fastest is reported.
const rounds = 3

// sink keeps the control loop from being optimised away.
var sink uint64

// control is the fixed amount of integer arithmetic every case is divided by,
// so the machine's speed and its load divide out of the reported index. It is
// the same loop the crypto signing benchmark uses.
func control() {
	accumulator := uint64(1)
	for i := 0; i < 20_000_000; i++ {
		accumulator = accumulator*6364136223846793005 + 1442695040888963407
	}
	sink = accumulator
}

// measure returns the fastest of rounds timed rounds, in nanoseconds. Noise can
// only ever make a round slower, so the fastest is the least contaminated.
func measure(body func()) time.Duration {
	body()
	best := time.Duration(1<<63 - 1)
	for round := 0; round < rounds; round++ {
		start := time.Now()
		body()
		if elapsed := time.Since(start); elapsed < best {
			best = elapsed
		}
	}
	return best
}

func report(name string, body func()) {
	fmt.Printf("%s\t%d\n", name, int64(measure(body)))
}

const signVerifyRounds = 24

func main() {
	report("control/spin-fixed-work", control)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256([]byte("cg12 placement benchmark"))
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		panic(err)
	}

	report("p256/sign-verify", func() {
		for i := 0; i < signVerifyRounds; i++ {
			s, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
			if err != nil {
				panic(err)
			}
			if !ecdsa.VerifyASN1(&key.PublicKey, digest[:], s) {
				panic("signature did not verify")
			}
		}
	})
	report("p256/verify", func() {
		for i := 0; i < signVerifyRounds; i++ {
			if !ecdsa.VerifyASN1(&key.PublicKey, digest[:], signature) {
				panic("signature did not verify")
			}
		}
	})
}
