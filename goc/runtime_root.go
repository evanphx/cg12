package goc

// runtimeRootName and runtimeRootSource are the fixed root of a prebuilt runtime
// module.
//
// It imports nothing, and that is a deliberate limit rather than a placeholder.
// The prebuilt module leaves the interface-method dispatchers undefined for the
// program to supply, so every dispatcher the prebuilt module *calls* has to be one
// that every program module will generate. A program's reachable set always
// contains the runtime's -- the runtime roots are fixed and a program only adds to
// them -- so a runtime-only root is exactly the largest set for which that holds.
// Pull `fmt` into the root and the module starts calling fmt_Stringer_String,
// which a program that never imports fmt would not define, and the link fails.
//
// The cost is bounded and it is not the interesting part of the corpus: the
// sepcompile spike measured hello.go's whole 1.7 MB of .text as the runtime
// closure, 61% of a fmt program's functions and 25% of a net/http program's. A
// program that needs more compiles the difference itself, which is what makes the
// subtraction degrade gracefully.
const (
	runtimeRootName = "gocruntime.go"

	runtimeRootSource = `package main

func main() {}
`
)
