package goc

import (
	"fmt"
	"go/ast"
	"go/token"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

// The AST walk's half of the escape diagnostic. opt/escapediag.go carries the
// design; this is the part that watches the walk decide.
//
// The walk answers one question -- "can this object's address outlive this
// frame" -- by climbing the syntax around the object and, where the climb meets
// a call, by asking the same question of the callee's parameter. Its answer is a
// bool, and a bool is what every caller wanted until now. What it throws away is
// which step of that climb answered, and for an escape that is the whole of the
// useful information.
//
// So the walk deposits its answer here as it gives it, and the emitter harvests
// it at the same instant it records the placement. Nothing recomputes anything:
// escapeExplanation.Rule is written by the branch that returned, from inside that
// branch, and if that branch is ever changed to decide differently the string
// moves with it.
//
// # First write wins
//
// The walk short-circuits: the first branch on a failing path that names itself
// is the deepest one reached, and the deepest one is the one that actually
// decided. Later, shallower branches on the way back out therefore must not
// overwrite it -- they add themselves to Chain instead, which builds the path
// from the deciding use outwards.
//
// # Cost when off
//
// gen.escapeDiag is nil. Every hook is a nil check on a pointer field, and every
// message is built inside a closure the hook never calls -- so no string is
// formatted, no types.Object is stringified, and nothing is allocated. The
// closures themselves do not escape the hooks and stay on the stack.

// escapeExplanation is why the walk answered as it did about one object.
type escapeExplanation struct {
	// Rule is the branch that decided, in its own words. Empty means the walk
	// found nothing that publishes the object, which is the frame answer and has
	// no single rule to name.
	Rule string
	// Use is the position of the publication the walk found.
	Use token.Pos
	// Chain is the sequence of questions between the object and Use, from the use
	// outwards. Level 2 only; at level 1 it is never appended to.
	Chain []string
}

// escapeDiagnostics is the walk's explanation state, live only while the
// diagnostic is on.
type escapeDiagnostics struct {
	level int
	// depth is the nesting of escape questions in flight. A question asked while
	// another is in flight is a step of that one, not a new one, so only the
	// outermost resets.
	depth int
	// pending is the explanation being assembled. It stays readable after the
	// outermost question returns, which is when the emitter harvests it.
	pending escapeExplanation
}

// newEscapeDiagnostics returns the walk's explanation state, or nil when the
// diagnostic is off.
func newEscapeDiagnostics() *escapeDiagnostics {
	level := opt.EscapeDiagLevel()
	if level < 1 {
		return nil
	}
	return &escapeDiagnostics{level: level}
}

// diagQuestion brackets one escape question. It returns the explanation state to
// restore if the question answers "does not escape".
//
// The outermost question starts from nothing: a previous question whose answer
// nobody harvested must not explain this one. A nested question starts from
// whatever its caller has found so far, which is what makes first-write-wins
// mean "the deepest branch reached", not "the first branch ever run".
func (g *gen) diagQuestion() escapeExplanation {
	if g.escapeDiag == nil {
		return escapeExplanation{}
	}
	if g.escapeDiag.depth == 0 {
		g.escapeDiag.pending = escapeExplanation{}
	}
	g.escapeDiag.depth++
	return g.escapeDiag.pending
}

// diagResolve closes the question diagQuestion opened.
//
// A question that answered "does not escape" decided nothing, so whatever it
// recorded on the way is dropped and its caller carries on from where it was.
// One that answered "escapes" keeps what it found and, at level 2, adds itself
// to the chain -- which is how the chain comes out ordered from the deciding use
// outwards to the object.
//
// step is nil for a question that is not worth a line of its own.
func (g *gen) diagResolve(saved escapeExplanation, escaped bool, step func() string) {
	if g.escapeDiag == nil {
		return
	}
	g.escapeDiag.depth--
	if !escaped {
		g.escapeDiag.pending = saved
		return
	}
	if step == nil || g.escapeDiag.level < 2 || g.escapeDiag.pending.Rule == "" {
		return
	}
	g.escapeDiag.pending.Chain = append(g.escapeDiag.pending.Chain, step())
}

// diagRule names the branch that decided the question in flight. The first
// caller on a path wins, because the walk short-circuits and the first branch to
// name itself is the deepest one reached.
//
// build is only called when the diagnostic is on, so a message that costs a
// FullName or a Sprintf costs nothing off.
func (g *gen) diagRule(build func() string) {
	if g.escapeDiag == nil || g.escapeDiag.pending.Rule != "" {
		return
	}
	g.escapeDiag.pending.Rule = build()
}

// diagRuleOverride names a rule that supersedes whatever the walk found below
// it, for the one case where a question's answer is not the answer of the climb
// inside it: the loop rule, which asks about the loop body and then says
// something the climb never asked about. What the climb found becomes a chain
// step rather than being dropped.
func (g *gen) diagRuleOverride(build func() string) {
	if g.escapeDiag == nil {
		return
	}
	previous := g.escapeDiag.pending.Rule
	g.escapeDiag.pending.Rule = build()
	if previous != "" && g.escapeDiag.level >= 2 {
		g.escapeDiag.pending.Chain = append(g.escapeDiag.pending.Chain, "within the loop body: "+previous)
	}
}

// diagUse records where the deciding use is written. Like diagRule, first wins.
func (g *gen) diagUse(node ast.Node) {
	if g.escapeDiag == nil || g.escapeDiag.pending.Use.IsValid() || node == nil {
		return
	}
	g.escapeDiag.pending.Use = node.Pos()
}

// escapeWhy takes the explanation of the question that just finished. It is
// called by the emitter at the point it records the placement, so that what is
// reported is the answer to the question that placed the object and not to some
// later one.
//
// A heap placement whose walk named no rule gets one here rather than being
// reported with a blank: some branch decided and did not say so, and a reader
// needs to be told that much rather than nothing.
func (g *gen) escapeWhy(placement ir.AllocPlacement) escapeExplanation {
	if g.escapeDiag == nil {
		return escapeExplanation{}
	}
	why := g.escapeDiag.pending
	g.escapeDiag.pending = escapeExplanation{}
	if placement == ir.AllocInFrame {
		// A frame placement is the absence of a publication, not the presence of
		// a rule. gc says only "does not escape" here and so does this.
		return escapeExplanation{}
	}
	if why.Rule == "" {
		why.Rule = "the walk found a use it could not prove local"
	}
	return why
}

// placedFor turns a harvested explanation into the fields ir.PlacedAlloc carries.
func (g *gen) placedFor(why escapeExplanation) (rule string, use ir.SrcPos, chain []string) {
	if why.Rule == "" {
		return "", ir.SrcPos{}, nil
	}
	return why.Rule, g.srcPos(why.Use), why.Chain
}

// srcPos converts a go/token position into the module's, for a diagnostic
// record.
//
// It looks the file name up rather than interning it. ir.Module.File appends to
// the module's position-file table, and a diagnostic must not add anything to
// the module -- not even a table entry that no instruction points at. Every file
// the compiler emitted code from is in the table already, so a lookup that
// misses names a file with no code in it, and the record carries no position
// rather than an invented one.
func (g *gen) srcPos(position token.Pos) ir.SrcPos {
	if !position.IsValid() || g.fset == nil || g.mod == nil {
		return ir.SrcPos{}
	}
	at := g.fset.Position(position)
	file := uint32(0)
	for index, name := range g.mod.Files {
		if name == at.Filename {
			file = uint32(index + 1)
			break
		}
	}
	if file == 0 {
		return ir.SrcPos{}
	}
	return ir.SrcPos{
		File: file,
		Line: uint32(at.Line),
		Col:  uint32(at.Column),
	}
}

// diagPosition renders a position for a chain step.
func (g *gen) diagPosition(position token.Pos) string {
	if !position.IsValid() || g.fset == nil {
		return "?"
	}
	return g.fset.Position(position).String()
}

// parameterName names a parameter for a chain step: its own name where it has
// one, and its position where it does not.
func parameterName(name string, index int) string {
	if name == "" || name == "_" {
		return fmt.Sprintf("parameter %d", index)
	}
	return fmt.Sprintf("parameter %s", name)
}
