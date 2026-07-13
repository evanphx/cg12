package cc

import (
	"github.com/evanphx/cg12/ir"
	"modernc.org/cc/v4"
)

func (g *gen) genCompound(cs *cc.CompoundStatement) {
	if cs == nil {
		return
	}
	g.push()
	defer g.pop()
	for l := cs.BlockItemList; l != nil; l = l.BlockItemList {
		g.genBlockItem(l.BlockItem)
		if g.terminated() {
			break // the rest of the block is unreachable
		}
	}
}

func (g *gen) genBlockItem(bi *cc.BlockItem) {
	switch bi.Case {
	case cc.BlockItemDecl:
		g.genLocalDecl(bi.Declaration)
	case cc.BlockItemStmt:
		g.genStmt(bi.Statement)
	}
}

func (g *gen) genLocalDecl(d *cc.Declaration) {
	if d.Case != cc.DeclarationDecl {
		return
	}
	for l := d.InitDeclaratorList; l != nil; l = l.InitDeclaratorList {
		id := l.InitDeclarator
		dcl := id.Declarator
		if dcl == nil || dcl.IsSynthetic() || dcl.IsStatic() {
			continue // synthetic (__func__) and static locals need no stack slot
		}
		t := dcl.Type()
		if t.Kind() == cc.Function {
			continue // a local prototype
		}
		addr := g.cur.Alloc(align(t), int(t.Size()))
		g.setName(addr, dcl.Name()+".addr")
		g.define(dcl.Name(), lval{addr, t})
		if id.Case == cc.InitDeclaratorInit {
			g.genInit(addr, t, id.Initializer)
		}
	}
}

// genInit stores an initializer into the storage at addr.
func (g *gen) genInit(addr ir.Ref, t cc.Type, init *cc.Initializer) {
	if init.Case != cc.InitializerExpr {
		return // brace initializers are not yet supported
	}
	e := init.AssignmentExpression
	// char buf[] = "literal": copy the bytes into the array.
	if at, ok := t.(*cc.ArrayType); ok {
		if s, ok := e.Value().(cc.StringValue); ok {
			b := append([]byte(s), 0)
			for i, c := range b {
				if int64(i) >= at.Len() {
					break
				}
				g.cur.StoreSub(ir.SubB, g.fn.Word(int64(c)), g.offset(addr, i))
			}
			return
		}
	}
	val := g.convert(g.genExpr(e), e.Type(), t)
	g.storeVal(addr, val, t)
}

func (g *gen) genStmt(s *cc.Statement) {
	if s == nil {
		return
	}
	switch s.Case {
	case cc.StatementExpr:
		if s.ExpressionStatement != nil && s.ExpressionStatement.ExpressionList != nil {
			g.genExpr(s.ExpressionStatement.ExpressionList)
		}
	case cc.StatementCompound:
		g.genCompound(s.CompoundStatement)
	case cc.StatementSelection:
		g.genSelection(s.SelectionStatement)
	case cc.StatementIteration:
		g.genIteration(s.IterationStatement)
	case cc.StatementJump:
		g.genJump(s.JumpStatement)
	case cc.StatementLabeled:
		if s.LabeledStatement != nil {
			g.genStmt(s.LabeledStatement.Statement)
		}
	}
}

// genCond evaluates a controlling expression to a value that Jnz treats as true
// when nonzero (comparisons already yield 0/1; floats are compared against 0).
func (g *gen) genCond(e cc.ExpressionNode) ir.Ref {
	v := g.genExpr(e)
	if isFloat(e.Type()) {
		return g.cur.Cmp(ir.CmpFne, clsOf(e.Type()), v, g.fn.Double(0))
	}
	return v
}

func (g *gen) genSelection(ss *cc.SelectionStatement) {
	if ss.Case == cc.SelectionStatementSwitch {
		return // switch is not yet supported
	}
	cond := g.genCond(ss.ExpressionList)
	thenB := g.block("then")
	endB := g.block("endif")
	elseB := endB
	if ss.Case == cc.SelectionStatementIfElse {
		elseB = g.block("else")
	}
	g.cur.Jnz(cond, thenB, elseB)

	g.cur = thenB
	g.genStmt(ss.Statement)
	if !g.terminated() {
		g.cur.Goto(endB)
	}
	if ss.Case == cc.SelectionStatementIfElse {
		g.cur = elseB
		g.genStmt(ss.Statement2)
		if !g.terminated() {
			g.cur.Goto(endB)
		}
	}
	g.cur = endB
}

func (g *gen) genIteration(is *cc.IterationStatement) {
	switch is.Case {
	case cc.IterationStatementWhile:
		condB, bodyB, endB := g.block("cond"), g.block("body"), g.block("end")
		g.cur.Goto(condB)
		g.cur = condB
		g.cur.Jnz(g.genCond(is.ExpressionList), bodyB, endB)
		g.loopBody(is.Statement, bodyB, condB, endB, condB)
		g.cur = endB
	case cc.IterationStatementDo:
		bodyB, condB, endB := g.block("do"), g.block("cond"), g.block("end")
		g.cur.Goto(bodyB)
		g.loopBody(is.Statement, bodyB, condB, endB, condB)
		g.cur = condB
		g.cur.Jnz(g.genCond(is.ExpressionList), bodyB, endB)
		g.cur = endB
	case cc.IterationStatementFor, cc.IterationStatementForDecl:
		g.push()
		// The field layout differs: a for with a declaration puts the condition in
		// ExpressionList and the post-expression in ExpressionList2, whereas a plain
		// for uses ExpressionList (init), ExpressionList2 (cond), ExpressionList3 (post).
		var condE, postE cc.ExpressionNode
		if is.Case == cc.IterationStatementForDecl {
			g.genLocalDecl(is.Declaration)
			condE, postE = is.ExpressionList, is.ExpressionList2
		} else {
			if is.ExpressionList != nil {
				g.genExpr(is.ExpressionList)
			}
			condE, postE = is.ExpressionList2, is.ExpressionList3
		}
		condB, bodyB, postB, endB := g.block("cond"), g.block("body"), g.block("post"), g.block("end")
		g.cur.Goto(condB)
		g.cur = condB
		if condE != nil {
			g.cur.Jnz(g.genCond(condE), bodyB, endB)
		} else {
			g.cur.Goto(bodyB)
		}
		g.loopBody(is.Statement, bodyB, postB, endB, postB)
		g.cur = postB
		if postE != nil {
			g.genExpr(postE)
		}
		g.cur.Goto(condB)
		g.cur = endB
		g.pop()
	}
}

// loopBody emits a loop body: the body runs in bodyB, break jumps to endB,
// continue jumps to contB, and (if the body falls through) it flows to next.
func (g *gen) loopBody(body *cc.Statement, bodyB, contB, endB, next *ir.Block) {
	g.cur = bodyB
	g.brk = append(g.brk, endB)
	g.cont = append(g.cont, contB)
	g.genStmt(body)
	g.brk = g.brk[:len(g.brk)-1]
	g.cont = g.cont[:len(g.cont)-1]
	if !g.terminated() {
		g.cur.Goto(next)
	}
}

func (g *gen) genJump(js *cc.JumpStatement) {
	switch js.Case {
	case cc.JumpStatementReturn:
		if js.ExpressionList == nil {
			g.cur.RetVoid()
			return
		}
		v := g.convert(g.genExpr(js.ExpressionList), js.ExpressionList.Type(), g.retType())
		g.cur.Ret(v)
	case cc.JumpStatementBreak:
		if len(g.brk) > 0 {
			g.cur.Goto(g.brk[len(g.brk)-1])
		}
	case cc.JumpStatementContinue:
		if len(g.cont) > 0 {
			g.cur.Goto(g.cont[len(g.cont)-1])
		}
	}
}

// retType returns the current function's C return type class as a synthetic cc
// type is unavailable; we approximate with the function's IR return class.
func (g *gen) retType() cc.Type { return g.curRet }
