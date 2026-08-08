package ir

import (
	"fmt"
	"strings"
)

// AggTypeInterner canonicalises structurally identical aggregate types onto one
// pointer.
//
// It exists because a unit of the binary format carries its own type table: the
// whole aggregate-type closure reachable from whatever it holds. Decode two
// units and the same Go type comes back as two AggTypes, and the module ends up
// carrying a duplicate descriptor for each -- retained, because the decoded
// bodies point at them. For a per-function cache that is one type closure per
// function of the program.
//
// This is goc's function-cache merge logic, moved here so the decoder can use
// it too. It was written and proved there first: see functionCache in
// goc/functionmerge.go, which now delegates.
type AggTypeInterner struct {
	byKey map[string]*AggType
	// interning breaks the cycle a self-referential type would otherwise make of
	// the depth-first walk -- a linked-list node whose field is a pointer to
	// itself is ordinary Go.
	interning map[*AggType]bool
}

func NewAggTypeInterner() *AggTypeInterner {
	return &AggTypeInterner{
		byKey:     map[string]*AggType{},
		interning: map[*AggType]bool{},
	}
}

// Intern returns the canonical pointer for a structurally identical aggregate,
// adopting this one if it is the first of its shape.
//
// The nested field types are canonicalised first, and that is not a refinement:
// collectTypes walks Field.Type as well as the top-level references, so an
// aggregate whose *field* type is a second copy of a type the module already has
// encodes one more entry in the type table than a cold compile did. That is
// exactly what the first attempt at this got wrong -- two collected types cold
// against three warm, on a function whose printed IL was identical.
func (i *AggTypeInterner) Intern(aggregate *AggType) *AggType {
	if aggregate == nil || i.interning[aggregate] {
		return aggregate
	}
	i.interning[aggregate] = true
	for index := range aggregate.Fields {
		aggregate.Fields[index].Type = i.Intern(aggregate.Fields[index].Type)
	}
	for union := range aggregate.Cases {
		for index := range aggregate.Cases[union] {
			aggregate.Cases[union][index].Type = i.Intern(aggregate.Cases[union][index].Type)
		}
	}
	delete(i.interning, aggregate)
	key := aggTypeKey(aggregate)
	if existing, known := i.byKey[key]; known {
		return existing
	}
	i.byKey[key] = aggregate
	return aggregate
}

// InternFunc rewrites every aggregate reference hanging off a function to the
// canonical pointer.
//
// The six places an *AggType can hang off a function are collectTypes' list, and
// they are read from it rather than rediscovered: the encoder already has to
// know every one of them, and a merge that missed one would leave a duplicate
// type behind in exactly the case the encoder does not.
func (i *AggTypeInterner) InternFunc(function *Func) {
	if function == nil {
		return
	}
	function.RetAgg = i.Intern(function.RetAgg)
	for index := range function.AggregateValues {
		function.AggregateValues[index].Type = i.Intern(function.AggregateValues[index].Type)
	}
	for index := range function.ParamGroups {
		function.ParamGroups[index].Type = i.Intern(function.ParamGroups[index].Type)
	}
	for _, temporary := range function.Temps {
		if temporary != nil {
			temporary.Agg = i.Intern(temporary.Agg)
		}
	}
	for _, block := range function.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			instruction.RetAgg = i.Intern(instruction.RetAgg)
			for argument := range instruction.AggArgs() {
				instruction.AggArgs()[argument] = i.Intern(instruction.AggArgs()[argument])
			}
			for group := range instruction.ArgGroups() {
				instruction.ArgGroups()[group].Type = i.Intern(instruction.ArgGroups()[group].Type)
			}
		}
	}
}

// Seed adopts a module's own declared types, so that anything interned
// afterwards canonicalises onto the pointers the module already holds rather
// than the other way round.
func (i *AggTypeInterner) Seed(m *Module) {
	if m == nil {
		return
	}
	for _, aggregate := range m.Types {
		i.Intern(aggregate)
	}
}

// aggTypeKey is a structural spelling of an aggregate type, including its name.
//
// The name is part of it because goc derives an aggregate's name from the same
// key gen.goABITypes is keyed on, so two aggregates share a name exactly when a
// cold compile would have shared the pointer. Interning on structure alone would
// merge two aggregates a cold compile kept apart, and the merged module would
// stop matching the cold one for no gain.
func aggTypeKey(aggregate *AggType) string {
	var out strings.Builder
	var write func(*AggType, int)
	writeFields := func(fields []Field, depth int) {
		for _, field := range fields {
			fmt.Fprintf(&out, "f%d,%d,%v;", field.Sub, field.Count, field.Pointer)
			if field.Type != nil {
				write(field.Type, depth+1)
			}
		}
	}
	write = func(aggregate *AggType, depth int) {
		if aggregate == nil || depth > 16 {
			out.WriteString("<>")
			return
		}
		fmt.Fprintf(&out, "[%s|%d|%d|%v|%v|%v|", aggregate.Name, aggregate.Align, aggregate.Size,
			aggregate.Opaque, aggregate.Packed, aggregate.Union)
		writeFields(aggregate.Fields, depth)
		for _, union := range aggregate.Cases {
			out.WriteString("c:")
			writeFields(union, depth)
		}
		out.WriteString("]")
	}
	write(aggregate, 0)
	return out.String()
}
