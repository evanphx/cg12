package goc

import (
	"fmt"
	"sort"
	"strings"
)

// runtimeRootName is the file name of the fixed root of a prebuilt runtime
// module.
//
// The root is a `package main` with an empty body that blank-imports the
// packages the pack is asked to carry. Blank imports rather than uses, because
// what matters is the *closure* the front end loads: goc compiles the whole
// runtime closure for any executable, and every package in the root's import
// graph joins it.
//
// A root that imports nothing is the runtime alone. That was the original limit
// and the reason was real: the prebuilt module leaves the interface-method
// dispatchers undefined for the program to supply, so every dispatcher the
// prebuilt module *calls* has to be one the program module also generates. Pull
// `fmt` into the root and the pack starts calling fmt_Stringer_String, which a
// program that never imports fmt does not define, and the link fails naming a
// mangled symbol.
//
// stubUnsuppliedProgramSymbols removes that limit: a program module that does
// not generate a dispatcher the pack expects defines a stub instead. See its
// comment for why the stub is safe rather than merely convenient.
const runtimeRootName = "gocruntime.go"

// runtimeRootSourceFor builds the root program for a pack carrying packages.
//
// The import list is sorted and de-duplicated so the same request always
// produces the same source, which is what lets a pack's fingerprint identify it.
func runtimeRootSourceFor(packages []string) (string, error) {
	for _, path := range packages {
		if path == "" || strings.ContainsAny(path, "`\"\n") {
			return "", fmt.Errorf("goc: %q is not a usable import path for a prebuilt runtime", path)
		}
	}
	unique := append([]string(nil), packages...)
	sort.Strings(unique)
	var source strings.Builder
	source.WriteString("package main\n")
	written := map[string]bool{}
	for _, path := range unique {
		if written[path] {
			continue
		}
		written[path] = true
		fmt.Fprintf(&source, "\nimport _ %q", path)
	}
	source.WriteString("\n\nfunc main() {}\n")
	return source.String(), nil
}
