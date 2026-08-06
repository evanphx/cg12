package goc_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// TestIRVerifyAudit runs ir.Verify over every function of every corpus program
// and requires that none of them fails. There is no baseline file: unlike the
// escape and aliasing audits, which record what the compiler currently does so
// the record cannot drift, this one has a single acceptable answer. IR that
// fails the verifier is IR no pass is entitled to assume anything about.
//
// The test exists because that answer was not the one the tree gave. ir.Verify
// has been there for a while, and nothing on the path from the front end to the
// backend ever called it -- its only callers are ir.DecodeModule and the
// lifter, neither of which sees a goc compile. So goc emitted IR its own
// verifier rejected on 4.2% of functions (every closure, deferwrap, gowrap and
// methodvalue that read a captured variable), for as long as it took someone to
// try decoding a module and find out. The defect was in the verifier -- its
// model of function entry knew about parameters and not about the closure
// environment the ABI supplies; see ir.defineClosureContext -- but the reason it
// survived is that nothing ran it. This is what runs it.
//
// It is nearly free: one linear walk per function, inside a corpus pass whose
// cost is the compiles.
func TestIRVerifyAudit(t *testing.T) {
	audit := auditCorpus(t)

	require.NotZero(t, audit.functions, "the audit must have verified something")

	if len(audit.verifyFailures) == 0 {
		t.Logf("%d function verifications across %d programs, all clean", audit.functions, audit.programs)
		return
	}

	shown := sortedKeys(audit.verifyFailures)
	const limit = 40
	truncated := ""
	if len(shown) > limit {
		truncated = fmt.Sprintf("\n  ... and %d more", len(shown)-limit)
		shown = shown[:limit]
	}
	for index, failure := range shown {
		shown[index] = failure + "\n      first seen compiling: " + audit.verifyFailures[failure]
	}

	// The two counts have different denominators and are deliberately not
	// divided: a diagnostic is deduplicated across programs (the corpus shares
	// one stdlib, so the same function is compiled hundreds of times), while
	// audit.functions counts every verification. A ratio of the two would mean
	// nothing. To get a percentage, compile one program and count.
	t.Fatalf("goc emitted IR that ir.Verify rejects: %d distinct diagnostics, from %d function\n"+
		"verifications across %d programs (a diagnostic names one function and is counted once\n"+
		"however many programs share it).\n"+
		"Every pass downstream of the front end assumes what the verifier checks, so this is\n"+
		"either a front end emitting malformed IR or a verifier whose model of well-formed IR\n"+
		"is missing a case the front end legitimately produces. Decide which before changing\n"+
		"either, and state the invariant where the next person will find it.\n  %s%s",
		len(audit.verifyFailures), audit.functions, audit.programs,
		strings.Join(shown, "\n  "), truncated)
}

// TestModuleRoundTripsThroughTheBinaryFormat encodes a whole compiled module,
// decodes it, and re-encodes the result, both before and after the optimizer.
//
// ir.DecodeModule ends with VerifyModule and returns the error instead of a
// module, so this is the end-to-end statement of what TestIRVerifyAudit checks
// per function: until every function verified, no goc module could be written
// to disk and read back, and per-package IR caching had nothing to build on.
// The re-encode makes it a fixed point rather than merely a decode that did not
// error -- if the decoder dropped a field the encoder writes, the second blob
// would differ from the first.
//
// One program, not the corpus: the corpus pass proves the verifier's per-
// function claim over 400 programs, and this proves the format carries a whole
// module. The cost is the encode and decode of a hundred megabytes, so paying
// it once is enough.
func TestModuleRoundTripsThroughTheBinaryFormat(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a whole program and encodes it twice")
	}

	const program = "testdata/hello.go"
	source, err := os.ReadFile(program)
	require.NoError(t, err)

	module, err := goc.CompileExecutable(program, source)
	require.NoError(t, err)

	for _, stage := range []string{"as compiled", "optimized"} {
		if stage == "optimized" {
			opt.OptimizeModule(module)
		}
		t.Run(stage, func(t *testing.T) {
			require.NoError(t, ir.VerifyModule(module), "the module must verify before it can be encoded")

			blob, err := module.MarshalBinary()
			require.NoError(t, err)

			decoded, err := ir.DecodeModule(blob)
			require.NoError(t, err, "a whole module must survive the round trip; this is what IR caching needs")
			require.Equal(t, len(module.Funcs), len(decoded.Funcs))
			require.Equal(t, len(module.Data), len(decoded.Data))

			again, err := decoded.MarshalBinary()
			require.NoError(t, err)
			require.Equal(t, blob, again,
				"re-encoding the decoded module must reproduce the bytes; a difference is a field the decoder dropped")
		})
	}
}
