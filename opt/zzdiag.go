package opt

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// TEMPORARY diagnostic, deleted before this branch is done. GOC_DIAG_ESCAPE=<substr>
// prints, for every heap-allocation candidate in a function whose name or
// allocated type contains <substr>, where it landed and the first use that
// escaped it.

var diagFilter = os.Getenv("GOC_DIAG_ESCAPE")

var diagNoDeep = os.Getenv("GOC_DIAG_NODEEP") != ""

func diagCandidates(function *ir.Func, byName map[string]*ir.Func, facts *EscapeFacts, seeds []uint32) {
	if diagFilter == "" {
		return
	}
	escapes := analyzeCandidateEscapes(function, byName, facts, seeds, true)
	var lines []string
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.Op != ir.OHeapAlloc || instruction.To.Kind != ir.RefTemp {
				continue
			}
			typeName := constSymbolName(function, instruction.Args[1])
			if !strings.Contains(function.Name, diagFilter) && !strings.Contains(typeName, diagFilter) {
				continue
			}
			where := "frame"
			reason := ""
			if escapes.escapes(instruction.To.ID) {
				where = "heap"
				reason = escapes.reason(instruction.To.ID)
			}
			lines = append(lines, fmt.Sprintf("DIAG %s %d:%d %s %s %s",
				function.Name, instruction.Pos.Line, instruction.Pos.Col, typeName, where, reason))
		}
	}
	sort.Strings(lines)
	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
}

var diagFacts = os.Getenv("GOC_DIAG_FACTS")

var diagFuncs = map[string]*ir.Func{}

func diagDumpFacts(facts *EscapeFacts) {
	if diagFacts == "" || facts == nil {
		return
	}
	var names []string
	for name := range facts.params {
		if strings.Contains(name, diagFacts) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		for index, fact := range facts.params[name] {
			pname := ""
			if fn := diagFuncs[name]; fn != nil && index < len(fn.Params) && fn.Params[index] != nil {
				pname = fn.Params[index].Name
			}
			fmt.Fprintf(os.Stderr, "FACT %s param %d %q = %s deep=%v result=%d\n", name, index, pname, fact.Escape, fact.Deep, fact.Result)
		}
	}
}
