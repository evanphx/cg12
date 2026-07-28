package goc

import (
	"fmt"
	"go/token"
	"os"
	"sort"
)

// WriteBarrierDecision names what the frontend did with one store of a value
// that has at least one pointer word.
type WriteBarrierDecision string

const (
	// WriteBarrierEmitted means the store publishes its pointer words through a
	// barrier: goc_storep for a scalar or aggregate store, or a typed runtime
	// helper for a bulk operation.
	WriteBarrierEmitted WriteBarrierDecision = "emitted"
	// WriteBarrierElided means the store writes its pointer words directly.
	WriteBarrierElided WriteBarrierDecision = "elided"
)

// Reasons a barrier was emitted or elided. Each names one branch of the
// frontend's decision, so an audit line says which rule fired rather than only
// what the outcome was.
const (
	WriteBarrierReasonPointerStore     = "pointer-store"
	WriteBarrierReasonAggregateStore   = "aggregate-store"
	WriteBarrierReasonTypedSliceCopy   = "typedslicecopy"
	WriteBarrierReasonTypedMemClear    = "memclrHasPointers"
	WriteBarrierReasonStackDestination = "stack-destination"
	WriteBarrierReasonNotInHeap        = "not-in-heap-pointer"
	WriteBarrierReasonNoWriteBarrier   = "nowritebarrier-pragma"
	WriteBarrierReasonNoRuntimeHeap    = "runtime-allocation-disabled"
)

// WriteBarrierRecord is one store site's decision.
type WriteBarrierRecord struct {
	// Function is the name of the function being lowered.
	Function string
	// Position is the source position of the store, empty when the store is
	// compiler-synthesized and has no Go source behind it.
	Position string
	// ValueType is the Go type being stored.
	ValueType string
	// Decision is whether a barrier was emitted.
	Decision WriteBarrierDecision
	// Reason names the branch that decided it.
	Reason string
}

// writeBarrierAudit collects every store decision made during one compilation.
// It is whole-compilation state on gen, so a derived generator records into the
// same audit as the generator that derived it.
//
// The audit exists because an elided barrier leaves nothing in the emitted IR:
// it is an ordinary store, indistinguishable from a store of a scalar. Section
// 5.7 and section 5.8 both found barriers emitted where upstream emits none,
// and both were visible in the output; a barrier *missing* where upstream emits
// one would not be. Recording the decision at the point it is made is what
// makes the omission direction checkable at all. See RUNTIME_PLAN.md section 6.
type writeBarrierAudit struct {
	records []WriteBarrierRecord
}

func (a *writeBarrierAudit) record(record WriteBarrierRecord) {
	if a == nil {
		return
	}
	a.records = append(a.records, record)
}

// recordWriteBarrier adds one decision for the store currently being lowered.
func (g *gen) recordWriteBarrier(
	position token.Pos,
	valueType string,
	decision WriteBarrierDecision,
	reason string,
) {
	if g.writeBarrierAudit == nil {
		return
	}
	// The function being lowered is named by gen.functionName for the functions
	// reachability drives, but the main package's own declarations are lowered
	// without one, so fall back to the ir.Func's name.
	name := g.functionName
	if name == "" && g.fn != nil {
		name = g.fn.Name
	}
	recorded := WriteBarrierRecord{
		Function:  name,
		ValueType: valueType,
		Decision:  decision,
		Reason:    reason,
	}
	if position.IsValid() && g.fset != nil {
		recorded.Position = g.fset.Position(position).String()
	}
	g.writeBarrierAudit.record(recorded)
}

// WriteBarrierAuditEnabled reports whether the opt-in audit is on. It is off by
// default and costs nothing when off, in the spirit of GOC_DEBUG_NOSPLIT.
func WriteBarrierAuditEnabled() bool {
	return os.Getenv("GOC_DEBUG_WRITEBARRIER") != ""
}

// reportWriteBarrierAudit prints the collected decisions when the debug mode is
// on. Records are grouped by function so a reader can see one function's whole
// set of stores together.
func reportWriteBarrierAudit(audit *writeBarrierAudit) {
	if audit == nil || !WriteBarrierAuditEnabled() {
		return
	}
	emitted := 0
	for _, record := range audit.records {
		if record.Decision == WriteBarrierEmitted {
			emitted++
		}
	}
	fmt.Fprintf(
		os.Stderr,
		"goc: write barrier audit: %d pointer stores, %d emitted, %d elided\n",
		len(audit.records),
		emitted,
		len(audit.records)-emitted,
	)
	ordered := make([]WriteBarrierRecord, len(audit.records))
	copy(ordered, audit.records)
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left].Function != ordered[right].Function {
			return ordered[left].Function < ordered[right].Function
		}
		return ordered[left].Position < ordered[right].Position
	})
	for _, record := range ordered {
		position := record.Position
		if position == "" {
			position = "<synthesized>"
		}
		fmt.Fprintf(
			os.Stderr,
			"goc: write barrier audit: %s %s %s %s %s\n",
			record.Decision,
			record.Reason,
			record.Function,
			record.ValueType,
			position,
		)
	}
}
