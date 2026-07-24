package cc

import (
	"fmt"
	"reflect"
	"sort"

	moderncc "github.com/evanphx/cg12/internal/cc"
	"github.com/evanphx/cg12/ir"
)

func (g *gen) genCompound(cs *moderncc.CompoundStatement) {
	if cs == nil {
		return
	}
	g.push()
	defer g.pop()
	// Open a lifetime scope for this block: its fixed-size locals' lifetime.start /
	// lifetime.end markers bracket exactly this block, letting the frame allocator
	// share a slot between blocks whose lifetimes are disjoint and letting mem2reg
	// bound a promoted local to its scope rather than the whole computed-goto mesh.
	if g.lifeOn {
		g.lifeScope = append(g.lifeScope, nil)
		g.lifeLabel = append(g.lifeLabel, g.compoundHasLabel(cs))
	}
	// A block that declares a VLA saves the stack pointer on entry and restores it
	// on exit, so the array is reclaimed when the scope ends (crucial for a VLA in
	// a loop, which would otherwise grow the stack each iteration).
	vla := blockDeclaresVLA(cs)
	if vla {
		g.vlaScope = append(g.vlaScope, g.cur.StackSave())
	}
	for l := cs.BlockItemList; l != nil; l = l.BlockItemList {
		// After a terminator the rest of the block is unreachable, but a label or
		// case ahead can resume a new block, so keep scanning for one. A declaration
		// in between still creates a local that resumed code may use (a variable
		// declared after a return, before a goto label), so process it too -- for its
		// storage; its initializer, being dead, is skipped in genLocalDecl.
		if g.terminated() && !labeledItem(l.BlockItem) && !declItem(l.BlockItem) {
			continue
		}
		g.genBlockItem(l.BlockItem)
	}
	if vla {
		if !g.terminated() {
			g.cur.StackRestore(g.vlaScope[len(g.vlaScope)-1])
		}
		g.vlaScope = g.vlaScope[:len(g.vlaScope)-1]
	}
	// Fall-through exit: end this block's locals before the scope closes. A block
	// that leaves by a jump (break/continue/return/goto) is already terminated and
	// ended its locals at the jump, so this is skipped for it.
	if g.lifeOn {
		if !g.terminated() {
			g.lifeEndScope(len(g.lifeScope) - 1)
		}
		g.lifeScope = g.lifeScope[:len(g.lifeScope)-1]
		g.lifeLabel = g.lifeLabel[:len(g.lifeLabel)-1]
	}
}

// compoundHasLabel reports whether a compound statement's subtree contains a goto
// label (a computed goto may branch to it, so a scope that contains one must not be
// lifetime-ended when a goto leaves an inner scope).
// collectAddrLabels records every label whose address is taken (`&&label`) anywhere
// in the function body. Those are the only labels a computed goto (`goto *p`) can
// reach, so only they may keep a scope alive across such a goto -- an internal label
// reached solely by ordinary goto does not. It runs before body generation, so the
// set is complete when scope decisions are made. The AST is walked by reflection over
// exported fields only, which reaches every subexpression while skipping the
// unexported metadata and parent back-pointers that would otherwise cycle.
func (g *gen) collectAddrLabels(root *moderncc.CompoundStatement) {
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Ptr:
			if v.IsNil() {
				return
			}
			if ue, ok := v.Interface().(*moderncc.UnaryExpression); ok && ue.Case == moderncc.UnaryExpressionLabelAddr {
				g.addrLabels[ue.Token2.SrcStr()] = true
			}
			walk(v.Elem())
		case reflect.Struct:
			t := v.Type()
			for i := 0; i < v.NumField(); i++ {
				if t.Field(i).PkgPath != "" {
					continue // unexported: metadata/back-pointers -- skip to avoid cycles
				}
				walk(v.Field(i))
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(root))
}

// compoundHasLabel reports whether a compound statement's subtree contains a label
// that a computed goto could target (an address-taken label). Such a scope must not
// be lifetime-ended when a `goto *p` leaves an inner scope, since the goto might land
// on that label. Labels reached only by ordinary goto do not count -- an inner scope
// holding only those is still fully exited by a computed goto.
func (g *gen) compoundHasLabel(cs *moderncc.CompoundStatement) bool {
	if !g.hasComputedGoto {
		return false // lifeLabel is only consulted at a computed goto
	}
	for l := cs.BlockItemList; l != nil; l = l.BlockItemList {
		if g.stmtHasAddrLabel(l.BlockItem) {
			return true
		}
	}
	return false
}

func (g *gen) stmtHasAddrLabel(bi *moderncc.BlockItem) bool {
	if bi.Case != moderncc.BlockItemStmt {
		return false
	}
	return g.walkAddrLabel(bi.Statement)
}

func (g *gen) walkAddrLabel(s *moderncc.Statement) bool {
	if s == nil {
		return false
	}
	switch s.Case {
	case moderncc.StatementLabeled:
		ls := s.LabeledStatement
		if ls.Case == moderncc.LabeledStatementLabel && g.addrLabels[ls.Token.SrcStr()] {
			return true
		}
		return g.walkAddrLabel(ls.Statement)
	case moderncc.StatementCompound:
		return g.compoundHasLabel(s.CompoundStatement)
	case moderncc.StatementSelection:
		return g.walkAddrLabel(s.SelectionStatement.Statement) || g.walkAddrLabel(s.SelectionStatement.Statement2)
	case moderncc.StatementIteration:
		return g.walkAddrLabel(s.IterationStatement.Statement)
	}
	return false
}

// lifeEndScope emits a lifetime.end for every alloca declared in lifeScope[i].
func (g *gen) lifeEndScope(i int) {
	for _, addr := range g.lifeScope[i] {
		g.cur.LifeEnd(addr)
	}
}

// blockDeclaresVLA reports whether a compound statement directly declares a
// variable-length array (a nested block has its own scope handling).
func blockDeclaresVLA(cs *moderncc.CompoundStatement) bool {
	for l := cs.BlockItemList; l != nil; l = l.BlockItemList {
		bi := l.BlockItem
		if bi.Case != moderncc.BlockItemDecl || bi.Declaration == nil || bi.Declaration.Case != moderncc.DeclarationDecl {
			continue
		}
		for il := bi.Declaration.InitDeclaratorList; il != nil; il = il.InitDeclaratorList {
			dcl := il.InitDeclarator.Declarator
			if dcl == nil || dcl.IsStatic() {
				continue
			}
			if at, ok := dcl.Type().(*moderncc.ArrayType); ok && at.IsVLA() {
				return true
			}
		}
	}
	return false
}

// vlaUnwind restores the stack pointer to the value saved when the VLA scope at
// the given depth was entered, reclaiming every VLA allocated since. It is used
// when break/continue jumps out of one or more VLA scopes.
func (g *gen) vlaUnwind(depth int) {
	if len(g.vlaScope) > depth {
		g.cur.StackRestore(g.vlaScope[depth])
	}
}

// labeledItem reports whether a block item contains a label (a goto/computed-goto
// target, or a switch case/default) anywhere within it, so a fresh block can
// resume from it even after a terminator. The label need not be at the item's
// head: an interpreter's opcode handlers, for instance, are labels nested inside
// a plain { } block reached only by computed goto, and skipping that block as
// unreachable would drop every handler. This mirrors labelsIn's traversal, since
// the same nesting hides a label from either.
func labeledItem(bi *moderncc.BlockItem) bool {
	return bi.Case == moderncc.BlockItemStmt && stmtHasLabel(bi.Statement)
}

// declItem reports whether a block item is a declaration (which creates a local
// whether or not it is reached, so it is processed even past a terminator).
func declItem(bi *moderncc.BlockItem) bool {
	return bi.Case == moderncc.BlockItemDecl
}

// stmtHasLabel reports whether a statement contains a label at any depth,
// descending the same compound/selection/iteration nesting labelsIn collects
// labels through.
func stmtHasLabel(s *moderncc.Statement) bool {
	if s == nil {
		return false
	}
	switch s.Case {
	case moderncc.StatementLabeled:
		return true
	case moderncc.StatementCompound:
		cs := s.CompoundStatement
		if cs == nil {
			return false
		}
		for l := cs.BlockItemList; l != nil; l = l.BlockItemList {
			if bi := l.BlockItem; bi.Case == moderncc.BlockItemStmt && stmtHasLabel(bi.Statement) {
				return true
			}
		}
	case moderncc.StatementSelection:
		return stmtHasLabel(s.SelectionStatement.Statement) ||
			stmtHasLabel(s.SelectionStatement.Statement2)
	case moderncc.StatementIteration:
		return stmtHasLabel(s.IterationStatement.Statement)
	}
	return false
}

// trailingExpr peels any labels off a statement and reports the expression
// underneath, which is a statement expression's value. The labels come back so
// the caller can start their blocks first: they are jump targets, and the
// expression is what the last of them runs.
func trailingExpr(s *moderncc.Statement) (labels []*moderncc.LabeledStatement, expr moderncc.ExpressionNode, ok bool) {
	for s != nil && s.Case == moderncc.StatementLabeled {
		ls := s.LabeledStatement
		if ls == nil {
			return nil, nil, false
		}
		labels = append(labels, ls)
		s = ls.Statement
	}
	if s == nil || s.Case != moderncc.StatementExpr {
		return nil, nil, false
	}
	es := s.ExpressionStatement
	if es == nil || es.ExpressionList == nil {
		return nil, nil, false
	}
	return labels, es.ExpressionList, true
}

// genStmtExpr evaluates a GNU statement expression ({ ... }): its statements run
// in a fresh scope, and the value of the whole expression is the value of the
// final expression statement.
func (g *gen) genStmtExpr(cs *moderncc.CompoundStatement) ir.Ref {
	if cs == nil {
		return ir.R
	}
	// A label inside a statement expression needs a block before the goto that
	// targets it is generated, exactly as one in a function body does. The
	// function-level collectLabels cannot see it: it walks statements, and a
	// statement expression hangs off an expression -- reaching it from there would
	// mean walking every expression node kind. Here the compound statement is
	// already in hand, and this runs before its body, which is what the
	// pre-allocation is for.
	//
	// They are dropped again on the way out, so only a jump within the statement
	// expression can reach them. Jumping in from outside is not legal C -- gcc
	// calls it "jump into statement expression" -- and leaving them behind would
	// let one resolve, and only when the goto came after the expression in the
	// source. A jump OUT of one targets a label the function level collected.
	for _, name := range g.collectInnerLabels(cs) {
		defer delete(g.labels, name)
	}
	g.push()
	defer g.pop()
	result := ir.R
	for l := cs.BlockItemList; l != nil; l = l.BlockItemList {
		bi := l.BlockItem
		if l.BlockItemList == nil && bi.Case == moderncc.BlockItemStmt && bi.Statement != nil {
			if labels, es, ok := trailingExpr(bi.Statement); ok {
				// The value is the final expression, which may sit behind labels:
				// `({ ...; done: x; })` is legal and its value is x. Testing for a
				// bare expression statement misses that and leaves the whole
				// statement expression with no value at all.
				for _, ls := range labels {
					g.genLabeled(ls)
				}
				result = g.genExpr(es)
				break
			}
		}
		g.genBlockItem(bi)
		if g.terminated() {
			break
		}
	}
	return result
}

func (g *gen) genBlockItem(bi *moderncc.BlockItem) {
	switch bi.Case {
	case moderncc.BlockItemDecl:
		g.at(bi.Declaration)
		g.genLocalDecl(bi.Declaration)
	case moderncc.BlockItemStmt:
		g.genStmt(bi.Statement)
	}
}

func (g *gen) genLocalDecl(d *moderncc.Declaration) {
	// `__auto_type v = e;` (a GNU extension whose type is the initializer's) is a
	// declaration of its own kind: the declarator and initializer hang directly off
	// the Declaration rather than an InitDeclaratorList. GCC's <stdatomic.h> builds
	// its generic atomic accessors out of it, so mishandling it turned every
	// atomic_load/store into a reference to an undefined symbol.
	if d.Case == moderncc.DeclarationAuto {
		dcl := d.Declarator
		if dcl == nil || dcl.IsSynthetic() {
			return
		}
		t := dcl.Type()
		addr := g.allocAligned(t, int(t.Size()))
		g.setName(addr, dcl.Name()+".addr")
		g.define(dcl.Name(), lval{addr: addr, typ: t})
		g.beginLife(addr)
		if !g.terminated() {
			g.genInit(addr, t, d.Initializer)
		}
		return
	}
	if d.Case != moderncc.DeclarationDecl {
		return
	}
	for l := d.InitDeclaratorList; l != nil; l = l.InitDeclaratorList {
		id := l.InitDeclarator
		dcl := id.Declarator
		if dcl == nil || dcl.IsSynthetic() {
			continue // synthetic (__func__) needs no storage
		}
		g.checkAtomicDecl(dcl)
		t := dcl.Type()
		if t.Kind() == moderncc.Function {
			continue // a local prototype
		}
		if dcl.IsExtern() {
			// A block-scope `extern` names a global object defined elsewhere; it has
			// no storage of its own and is reached through its symbol. Allocating a
			// local slot instead (as if it were an ordinary local) made a reference
			// like `char **envp = environ;` read uninitialized stack.
			g.define(dcl.Name(), lval{sym: dcl.Name(), typ: t, thread: dcl.IsThreadLocal()})
			continue
		}
		if dcl.IsStatic() {
			g.genStaticLocal(id, dcl, t)
			continue
		}
		if at, ok := t.(*moderncc.ArrayType); ok && at.IsVLA() {
			addr := g.vlaAlloc(at) // runtime-sized stack storage; no initializer allowed
			g.setName(addr, dcl.Name()+".addr")
			g.define(dcl.Name(), lval{addr: addr, typ: t})
			continue
		}
		size := int(t.Size())
		if isVaList(t) {
			size = vaListBytes // hold the target's larger va_list state, not a pointer
		}
		addr := g.allocAligned(t, size)
		g.setName(addr, dcl.Name()+".addr")
		g.define(dcl.Name(), lval{addr: addr, typ: t})
		g.beginLife(addr)
		if id.Case == moderncc.InitDeclaratorInit && !g.terminated() {
			g.genInit(addr, t, id.Initializer)
		}
	}
}

// beginLife opens the lifetime of a fixed-size local's stack slot: it emits a
// lifetime.start and records the slot in the innermost block scope so the matching
// lifetime.end is emitted when that scope is left. It is a no-op in a terminated
// (unreachable) block, where a marker would land after the block's jump.
func (g *gen) beginLife(addr ir.Ref) {
	if !g.lifeOn || len(g.lifeScope) == 0 || g.terminated() {
		return
	}
	g.cur.LifeStart(addr)
	top := len(g.lifeScope) - 1
	g.lifeScope[top] = append(g.lifeScope[top], addr)
}

// vlaAlloc allocates a variable-length array on the stack. Its size is the
// runtime length times the element size, rounded up to keep the stack 16-aligned;
// the storage lives until the function returns.
func (g *gen) vlaAlloc(at *moderncc.ArrayType) ir.Ref {
	raw := g.vlaBytes(at)
	up := g.cur.Add(ir.ClsP, raw, g.fn.ConstInt(ir.ClsP, 15))
	sz := g.cur.And(ir.ClsP, up, g.fn.ConstInt(ir.ClsP, ^int64(15)))
	return g.cur.AllocN(sz)
}

// vlaBytes computes a VLA type's total byte size at run time: the product of its
// variable dimensions and the innermost fixed element size.
func (g *gen) vlaBytes(at *moderncc.ArrayType) ir.Ref {
	e := at.SizeExpression()
	n := g.toPtr(g.genExpr(e), e.Type())
	if inner, ok := at.Elem().(*moderncc.ArrayType); ok && inner.IsVLA() {
		return g.cur.Mul(ir.ClsP, n, g.vlaBytes(inner))
	}
	return g.cur.Mul(ir.ClsP, n, g.fn.ConstInt(ir.ClsP, int64(at.Elem().Size())))
}

// genStaticLocal gives a static local its own module-level storage under a
// mangled name (so two functions may each declare "static int n") and binds the
// name to that symbol. Its constant initializer becomes the data image; C
// requires static initializers to be constant, so no code runs at entry.
func (g *gen) genStaticLocal(id *moderncc.InitDeclarator, dcl *moderncc.Declarator, t moderncc.Type) {
	g.nstatic++
	name := fmt.Sprintf("%s.static.%d", dcl.Name(), g.nstatic)
	data := &ir.Data{Name: name, Align: dataAlign(t)} // zero Linkage = internal
	if id.Case == moderncc.InitDeclaratorInit {
		data.Items = g.globalItems(t, id.Initializer)
	}
	if len(data.Items) == 0 {
		data.Items = []ir.DataItem{{Zero: int(t.Size())}}
	}
	g.mod.Data = append(g.mod.Data, data)
	g.define(dcl.Name(), lval{sym: name, typ: t})
}

// genInit stores an initializer into the storage at addr.
func (g *gen) genInit(addr ir.Ref, t moderncc.Type, init *moderncc.Initializer) {
	if init.Case == moderncc.InitializerInitList {
		// A brace initializer: zero the whole aggregate, then fill the elements
		// that are named. Every leaf carries its absolute offset, so nested
		// braces and designated initializers need no bookkeeping here.
		g.zeroFill(addr, int(t.Size()))
		g.genBraceInit(addr, init.InitializerList)
		return
	}
	e := init.AssignmentExpression
	// char buf[] = "literal": copy the bytes into the array.
	if isArray(t) {
		if s, ok := e.Value().(moderncc.StringValue); ok {
			g.initString(addr, t, s)
			return
		}
	}
	if isMemValue(t) { // struct/union/long-double/complex initialized from another value
		src := g.rval(e, t)
		g.copyAgg(addr, src, int(t.Size()))
		return
	}
	val := g.rval(e, t)
	g.storeVal(addr, val, t)
}

// complit materializes a compound literal (T){...}: it reserves storage, zeroes
// it, and runs the brace initializer, yielding the object's address and type so
// the literal can be read, indexed, or have its address taken like any lvalue.
func (g *gen) complit(n *moderncc.PostfixExpression) (ir.Ref, moderncc.Type) {
	// Use the declared type, not n.Type(): an array literal's expression type has
	// already decayed to a pointer, which would under-allocate the storage.
	t := n.TypeName.Type()
	addr := g.allocAligned(t, int(t.Size()))
	g.zeroFill(addr, int(t.Size()))
	g.genBraceInit(addr, n.InitializerList)
	return addr, t
}

// genBraceInit stores each leaf of a brace initializer at its absolute offset.
func (g *gen) genBraceInit(addr ir.Ref, il *moderncc.InitializerList) {
	for ; il != nil; il = il.InitializerList {
		in := il.Initializer
		if in.Case == moderncc.InitializerInitList {
			g.genBraceInit(addr, in.InitializerList)
			continue
		}
		et := in.Type()
		e := in.AssignmentExpression
		if f := in.Field(); f != nil && f.IsBitfield() {
			v := g.convert(g.genExpr(e), e.Type(), f.Type())
			g.writeBitfield(g.offset(addr, int(in.Offset())), v, f)
			continue
		}
		dst := g.offset(addr, int(in.Offset()))
		if isArray(et) {
			if s, ok := e.Value().(moderncc.StringValue); ok {
				g.initString(dst, et, s)
				continue
			}
		}
		val := g.rval(e, et)
		if isMemValue(et) { // a struct/union/long-double/complex element is a byte copy
			g.copyAgg(dst, val, int(et.Size()))
			continue
		}
		g.storeVal(dst, val, et)
	}
}

// initString copies a string literal (with its terminating NUL, truncated to
// fit) into a char array at addr.
func (g *gen) initString(addr ir.Ref, t moderncc.Type, s moderncc.StringValue) {
	at := t.(*moderncc.ArrayType)
	b := append([]byte(s), 0)
	for i, c := range b {
		if int64(i) >= at.Len() {
			break
		}
		g.cur.StoreSub(ir.SubB, g.fn.Word(int64(c)), g.offset(addr, i))
	}
}

// zeroFill writes size bytes of zero starting at addr, using the widest stores
// that fit so the instruction count stays proportional to the word count.
func (g *gen) zeroFill(addr ir.Ref, size int) {
	off := 0
	for size-off >= 8 {
		g.cur.Store(g.fn.Long(0), g.offset(addr, off))
		off += 8
	}
	if size-off >= 4 {
		g.cur.Store(g.fn.Word(0), g.offset(addr, off))
		off += 4
	}
	if size-off >= 2 {
		g.cur.StoreSub(ir.SubH, g.fn.Word(0), g.offset(addr, off))
		off += 2
	}
	if size-off >= 1 {
		g.cur.StoreSub(ir.SubB, g.fn.Word(0), g.offset(addr, off))
	}
}

func (g *gen) genStmt(s *moderncc.Statement) {
	if s == nil {
		return
	}
	g.at(s)
	switch s.Case {
	case moderncc.StatementExpr:
		if s.ExpressionStatement != nil && s.ExpressionStatement.ExpressionList != nil {
			g.genExpr(s.ExpressionStatement.ExpressionList)
		}
	case moderncc.StatementCompound:
		g.genCompound(s.CompoundStatement)
	case moderncc.StatementSelection:
		g.genSelection(s.SelectionStatement)
	case moderncc.StatementIteration:
		g.genIteration(s.IterationStatement)
	case moderncc.StatementJump:
		g.genJump(s.JumpStatement)
	case moderncc.StatementLabeled:
		g.genLabeled(s.LabeledStatement)
	case moderncc.StatementAsm:
		g.genAsm(s.AsmStatement)
	}
}

// genCond evaluates a controlling expression to a value that Jnz treats as true
// when nonzero (comparisons already yield 0/1; floats are compared against 0).
func (g *gen) genCond(e moderncc.ExpressionNode) ir.Ref {
	v := g.genExpr(e)
	if isFloat(e.Type()) {
		return g.cur.Cmp(ir.CmpFne, ir.ClsW, v, g.floatOf(0, e.Type()))
	}
	return v
}

func (g *gen) genSelection(ss *moderncc.SelectionStatement) {
	if ss.Case == moderncc.SelectionStatementSwitch {
		g.genSwitch(ss)
		return
	}
	cond := g.genCond(ss.ExpressionList)
	thenB := g.block("then")
	endB := g.block("endif")
	elseB := endB
	if ss.Case == moderncc.SelectionStatementIfElse {
		elseB = g.block("else")
	}
	g.cur.Jnz(cond, thenB, elseB)

	g.cur = thenB
	g.genStmt(ss.Statement)
	if !g.terminated() {
		g.cur.Goto(endB)
	}
	if ss.Case == moderncc.SelectionStatementIfElse {
		g.cur = elseB
		g.genStmt(ss.Statement2)
		if !g.terminated() {
			g.cur.Goto(endB)
		}
	}
	g.cur = endB
}

// switchCase pairs a case label's constant value with the block it dispatches to.
type switchCase struct {
	val int64
	blk *ir.Block
}

// genSwitch lowers a switch: a linear chain of equality tests dispatches the
// controlling value to the matching case block (or default, or the end), and the
// body is then generated so that cases fall through in source order and break
// exits to the end. Case blocks are pre-allocated by collectCases and shared with
// genLabeled, which begins each block when its label is reached in the body.
func (g *gen) genSwitch(ss *moderncc.SelectionStatement) {
	ct := ss.ExpressionList.Type()
	cond := g.genExpr(ss.ExpressionList)

	var cases []switchCase
	var defBlk *ir.Block
	g.collectCases(ss.Statement, &cases, &defBlk)

	endB := g.block("swend")
	deflt := defBlk
	if deflt == nil {
		deflt = endB
	}
	// Emit one multiway branch; a backend lowering pass chooses the dispatch
	// form (if-chain, binary search, or jump table).
	scases := make([]ir.SwitchCase, len(cases))
	for i, c := range cases {
		scases[i] = ir.SwitchCase{Val: c.val, Blk: c.blk}
	}
	g.cur.Switch(cond, deflt, signed(ct), scases)

	// Generate the body. The prologue block before the first label is
	// unreachable (dispatch jumps straight to the case blocks); dead code there
	// is dropped by the optimizer.
	g.cur = g.block("swbody")
	g.brk = append(g.brk, endB)
	g.brkVla = append(g.brkVla, len(g.vlaScope))
	g.brkLife = append(g.brkLife, len(g.lifeScope))
	g.genStmt(ss.Statement)
	g.brk = g.brk[:len(g.brk)-1]
	g.brkVla = g.brkVla[:len(g.brkVla)-1]
	g.brkLife = g.brkLife[:len(g.brkLife)-1]
	if !g.terminated() {
		g.cur.Goto(endB)
	}
	g.cur = endB
}

// collectCases walks a switch body, allocating a block for every case and
// default label and recording the case values. It descends through compounds,
// conditionals, and loops (so labels inside them are found) but not into a
// nested switch, whose labels belong to it.
func (g *gen) collectCases(s *moderncc.Statement, cases *[]switchCase, defBlk **ir.Block) {
	if s == nil {
		return
	}
	switch s.Case {
	case moderncc.StatementCompound:
		if cs := s.CompoundStatement; cs != nil {
			for l := cs.BlockItemList; l != nil; l = l.BlockItemList {
				if l.BlockItem.Case == moderncc.BlockItemStmt {
					g.collectCases(l.BlockItem.Statement, cases, defBlk)
				}
			}
		}
	case moderncc.StatementLabeled:
		ls := s.LabeledStatement
		switch ls.Case {
		case moderncc.LabeledStatementCaseLabel:
			v, ok := constInt(ls.ConstantExpression)
			if !ok {
				// Silently taking 0 puts the case somewhere the source never asked
				// for, and the switch still compiles and runs.
				g.fail("cc: case label is not a constant expression")
				return
			}
			blk := g.block("case")
			g.caseBlk[ls] = blk
			*cases = append(*cases, switchCase{v, blk})
		case moderncc.LabeledStatementRange: // GNU "case lo ... hi:"
			lo, okLo := constInt(ls.ConstantExpression)
			hi, okHi := constInt(ls.ConstantExpression2)
			if !okLo || !okHi {
				g.fail("cc: case range bound is not a constant expression")
				return
			}
			blk := g.block("case")
			g.caseBlk[ls] = blk
			if hi < lo {
				break // an inverted range matches nothing
			}
			const maxRange = 1 << 16
			if n := uint64(hi) - uint64(lo); n >= maxRange {
				g.fail("cc: case range %d ... %d spans too many values", lo, hi)
			} else {
				for i := uint64(0); i <= n; i++ {
					*cases = append(*cases, switchCase{lo + int64(i), blk})
				}
			}
		case moderncc.LabeledStatementDefault:
			blk := g.block("default")
			g.caseBlk[ls] = blk
			*defBlk = blk
		}
		g.collectCases(ls.Statement, cases, defBlk)
	case moderncc.StatementSelection:
		if s.SelectionStatement.Case == moderncc.SelectionStatementSwitch {
			return // a nested switch owns its own cases
		}
		g.collectCases(s.SelectionStatement.Statement, cases, defBlk)
		g.collectCases(s.SelectionStatement.Statement2, cases, defBlk)
	case moderncc.StatementIteration:
		g.collectCases(s.IterationStatement.Statement, cases, defBlk)
	}
}

// collectInnerLabels pre-allocates a block for every label in a statement
// expression, returning their names so the caller can drop them again: they are
// reachable only from within it.
func (g *gen) collectInnerLabels(cs *moderncc.CompoundStatement) []string {
	before := make(map[string]bool, len(g.labels))
	for n := range g.labels {
		before[n] = true
	}
	g.collectLabels(cs)
	var added []string
	for n := range g.labels {
		if !before[n] {
			added = append(added, n)
		}
	}
	return added
}

// collectLabels pre-allocates a block for every goto label in the function, so a
// forward goto can target a block that has not yet been generated.
func (g *gen) collectLabels(cs *moderncc.CompoundStatement) {
	if cs == nil {
		return
	}
	for l := cs.BlockItemList; l != nil; l = l.BlockItemList {
		if l.BlockItem.Case == moderncc.BlockItemStmt {
			g.labelsIn(l.BlockItem.Statement)
		}
	}
}

// labelBlocks returns every label's block, sorted by name for determinism. It is
// the conservative target set for a computed goto: any address-taken label is a
// possible destination, and listing the rest only over-approximates the CFG.
func (g *gen) labelBlocks() []*ir.Block {
	blocks := make([]*ir.Block, 0, len(g.labels))
	for _, b := range g.labels {
		blocks = append(blocks, b)
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].Name < blocks[j].Name })
	return blocks
}

func (g *gen) labelsIn(s *moderncc.Statement) {
	if s == nil {
		return
	}
	switch s.Case {
	case moderncc.StatementCompound:
		g.collectLabels(s.CompoundStatement)
	case moderncc.StatementLabeled:
		ls := s.LabeledStatement
		if ls.Case == moderncc.LabeledStatementLabel {
			g.labels[ls.Token.SrcStr()] = g.block("L." + ls.Token.SrcStr())
		}
		g.labelsIn(ls.Statement)
	case moderncc.StatementSelection:
		g.labelsIn(s.SelectionStatement.Statement)
		g.labelsIn(s.SelectionStatement.Statement2)
	case moderncc.StatementIteration:
		g.labelsIn(s.IterationStatement.Statement)
	case moderncc.StatementJump:
		if s.JumpStatement != nil && s.JumpStatement.Case == moderncc.JumpStatementGotoExpr {
			g.hasComputedGoto = true // a `goto *p`: lifetime scope-ending must respect address-taken labels
		}
	}
}

// genLabeled begins the block a label names — a goto target, or a switch case or
// default — flowing into it from the preceding code if that code falls through.
func (g *gen) genLabeled(ls *moderncc.LabeledStatement) {
	var target *ir.Block
	switch ls.Case {
	case moderncc.LabeledStatementLabel:
		target = g.labels[ls.Token.SrcStr()]
	case moderncc.LabeledStatementCaseLabel, moderncc.LabeledStatementRange, moderncc.LabeledStatementDefault:
		target = g.caseBlk[ls]
	}
	if target != nil {
		if !g.terminated() {
			g.cur.Goto(target)
		}
		g.cur = target
	}
	g.genStmt(ls.Statement)
}

func (g *gen) genIteration(is *moderncc.IterationStatement) {
	switch is.Case {
	case moderncc.IterationStatementWhile:
		condB, bodyB, endB := g.block("cond"), g.block("body"), g.block("end")
		g.cur.Goto(condB)
		g.cur = condB
		g.cur.Jnz(g.genCond(is.ExpressionList), bodyB, endB)
		g.loopBody(is.Statement, bodyB, condB, endB, condB)
		g.cur = endB
	case moderncc.IterationStatementDo:
		bodyB, condB, endB := g.block("do"), g.block("cond"), g.block("end")
		g.cur.Goto(bodyB)
		g.loopBody(is.Statement, bodyB, condB, endB, condB)
		g.cur = condB
		g.cur.Jnz(g.genCond(is.ExpressionList), bodyB, endB)
		g.cur = endB
	case moderncc.IterationStatementFor, moderncc.IterationStatementForDecl:
		g.push()
		// The field layout differs: a for with a declaration puts the condition in
		// ExpressionList and the post-expression in ExpressionList2, whereas a plain
		// for uses ExpressionList (init), ExpressionList2 (cond), ExpressionList3 (post).
		var condE, postE moderncc.ExpressionNode
		if is.Case == moderncc.IterationStatementForDecl {
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
func (g *gen) loopBody(body *moderncc.Statement, bodyB, contB, endB, next *ir.Block) {
	g.cur = bodyB
	g.brk = append(g.brk, endB)
	g.brkVla = append(g.brkVla, len(g.vlaScope))
	g.brkLife = append(g.brkLife, len(g.lifeScope))
	g.cont = append(g.cont, contB)
	g.contVla = append(g.contVla, len(g.vlaScope))
	g.contLife = append(g.contLife, len(g.lifeScope))
	g.genStmt(body)
	g.brk = g.brk[:len(g.brk)-1]
	g.brkVla = g.brkVla[:len(g.brkVla)-1]
	g.brkLife = g.brkLife[:len(g.brkLife)-1]
	g.cont = g.cont[:len(g.cont)-1]
	g.contVla = g.contVla[:len(g.contVla)-1]
	g.contLife = g.contLife[:len(g.contLife)-1]
	if !g.terminated() {
		g.cur.Goto(next)
	}
}

func (g *gen) genJump(js *moderncc.JumpStatement) {
	switch js.Case {
	case moderncc.JumpStatementReturn:
		if js.ExpressionList == nil {
			g.cur.RetVoid()
			return
		}
		v := g.rval(js.ExpressionList, g.retType())
		g.cur.Ret(v)
	case moderncc.JumpStatementBreak:
		if len(g.brk) > 0 {
			g.lifeUnwind(g.brkLife[len(g.brkLife)-1]) // end locals of scopes the break leaves
			g.vlaUnwind(g.brkVla[len(g.brkVla)-1])    // reclaim VLAs left by the break
			g.cur.Goto(g.brk[len(g.brk)-1])
		}
	case moderncc.JumpStatementContinue:
		if len(g.cont) > 0 {
			g.lifeUnwind(g.contLife[len(g.contLife)-1]) // end locals of scopes the continue leaves
			g.vlaUnwind(g.contVla[len(g.contVla)-1])    // reclaim VLAs left by the continue
			g.cur.Goto(g.cont[len(g.cont)-1])
		}
	case moderncc.JumpStatementGoto:
		// collectLabels pre-allocates a block for every label in the function, so a
		// forward goto resolves here too and a name that is still missing is one
		// that does not exist. Emitting nothing would fall through to whatever
		// follows -- a jump to the wrong place, silently.
		b, ok := g.labels[js.Token2.SrcStr()]
		if !ok {
			g.fail("cc: goto %s: no such label", js.Token2.SrcStr())
			return
		}
		g.cur.Goto(b)
	case moderncc.JumpStatementGotoExpr: // computed goto: goto *expr
		// Evaluate the target first, then end the locals of the scopes this goto
		// leaves: a computed goto may only branch to a label, so any innermost scope
		// whose subtree holds no label is fully exited and its locals are dead. Stop
		// at the first label-bearing scope (a target could resume inside it).
		target := g.genExpr(js.ExpressionList)
		g.lifeEndComputedGoto()
		g.cur.BrIndirect(target, g.labelBlocks()...)
	}
}

// lifeUnwind ends the locals of every block scope from the innermost down to (but
// not including) depth, in innermost-first order. It mirrors vlaUnwind and is used
// when a break/continue jumps out of one or more scopes.
func (g *gen) lifeUnwind(depth int) {
	if !g.lifeOn {
		return
	}
	for i := len(g.lifeScope) - 1; i >= depth; i-- {
		g.lifeEndScope(i)
	}
}

// lifeEndComputedGoto ends the locals of the innermost scopes a computed goto
// leaves: the contiguous innermost scopes whose subtree contains no label. It stops
// at the first label-bearing scope, which the goto might re-enter.
func (g *gen) lifeEndComputedGoto() {
	if !g.lifeOn {
		return
	}
	for i := len(g.lifeScope) - 1; i >= 0 && !g.lifeLabel[i]; i-- {
		g.lifeEndScope(i)
	}
}

// retType returns the current function's C return type class as a synthetic cc
// type is unavailable; we approximate with the function's IR return class.
func (g *gen) retType() moderncc.Type { return g.curRet }
