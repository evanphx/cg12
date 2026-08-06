package main

import (
	"os"

	"github.com/evanphx/cg12/internal/prebuilt"
)

// A pack can be consumed in three ways, and they trade compile time against code
// quality against each other. GOC_PACK_MODE selects which, so the three can be
// measured on one tree without rebuilding the compiler -- the same reason
// GOC_OPT_PIPELINE exists -- and so a regression can be attributed to the choice
// rather than to the pack machinery as a whole.
//
//	object   (the arrangement before this change) subtract the pack's definitions
//	         from the whole-program module, then optimize the ~600 functions that
//	         are left. Nothing can be inlined across the pack boundary: every
//	         callee in the pack is an external symbol by the time the optimizer
//	         runs. Cheapest, and the one that costs code quality.
//
//	compose  optimize the whole program -- pack functions included -- and subtract
//	         afterwards. The optimizer sees exactly the module a monolithic build
//	         sees, so program-local code is generated exactly as a monolithic build
//	         generates it. The pack's object is still what gets linked, so the back
//	         end still only lowers the program's own functions.
//
//	ir       compose, plus the pack's IR: the program module's copy of each packed
//	         function is replaced by the pack's own optimized body before the
//	         optimizer runs, so the passes converge on it instead of re-deriving
//	         it. This is the mode the pack's IR member exists for.
//
// **The default is compose, and the reason is measured.** ir is 3.5 s faster
// than compose on a warm compile of the float benchmark (11.57 s against
// 15.10 s), and it costs code quality to get it: the inliner splices a callee
// that has already been through unroll/ifconvert/tailmerge/gcm, where a
// monolithic build splices the callee as it stands at that round of the inline
// fixpoint and cleans up afterwards. Measured, `-perf-bench-only text` at nine
// repetitions:
//
//	arm         text/parse goc/host ratio   against the baseline
//	monolithic  7.7099                      -0.1%
//	object      7.9482                      +2.9%  (within a 5.0% tolerance)
//	compose     7.6764                      -0.6%
//	ir          8.1908                      +6.1%  PAST TOLERANCE
//
// compose reproduces the monolithic number; ir is worse than the object pack it
// replaces. A default that trades the thing this change exists to fix for a
// quarter of the compile time would be the wrong default, so ir is a named arm
// and not the default. Nothing writes the IR member unless it is selected, so an
// ordinary pack is the size it always was.
const packModeVariable = "GOC_PACK_MODE"

type packMode struct {
	name    string
	compose bool
	carryIR bool
}

var packModes = []packMode{
	{name: "object"},
	{name: "compose", compose: true},
	{name: "ir", compose: true, carryIR: true},
}

// selectedPackMode reads GOC_PACK_MODE. An unrecognized value panics rather than
// silently selecting the default, for the reason GOC_OPT_PIPELINE does: a typo in
// a measurement variable that quietly measures the arm you were trying to rule
// out is worse than a crash.
func selectedPackMode() packMode {
	requested := os.Getenv(packModeVariable)
	if requested == "" {
		requested = "compose"
	}
	for _, mode := range packModes {
		if mode.name == requested {
			return mode
		}
	}
	panic("goc: " + packModeVariable + "=" + requested + " is not one of object, compose, ir")
}

// packModeIdentity names the selected mode for a cache key.
func packModeIdentity() string { return selectedPackMode().name }

// applyPackMode fills the options a pack build and a program build take from the
// selected mode.
func applyPackMode(options prebuilt.Options) prebuilt.Options {
	mode := selectedPackMode()
	options.Compose = mode.compose
	options.CarryIR = mode.carryIR
	return options
}
