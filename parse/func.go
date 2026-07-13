package parse

import "github.com/evanphx/cg12/ir"

// --- functions ------------------------------------------------------------

func (p *parser) parseFunc(m *ir.Module, lk ir.Linkage) {
	p.expectWord() // "function"
	p.curPos = ir.SrcPos{}

	hasRet := false
	var retty ir.Cls
	var retAgg *ir.AggType
	switch {
	case p.tok.kind == tGlobal:
		// no return type (void)
	case p.tok.kind == tType:
		hasRet = true
		retAgg = p.typesByName[p.tok.s]
		p.advance()
	default:
		hasRet = true
		retty = p.parseClass()
	}
	name := p.expect(tGlobal, "function name").s

	var f *ir.Func
	if hasRet && retAgg == nil {
		f = m.NewFunc(name, retty)
	} else {
		f = m.NewFuncVoid(name)
		if retAgg != nil {
			f.HasRet = true
			f.RetAgg = retAgg
		}
	}
	f.Linkage = lk

	p.f = f
	p.temps = map[string]ir.Ref{}
	p.blocks = map[string]*ir.Block{}
	p.appended = map[*ir.Block]bool{}

	p.parseParams()
	p.expectPunct("{")
	for p.err == nil && !p.acceptPunct("}") {
		p.parseBlock()
	}
}

func (p *parser) parseParams() {
	p.expectPunct("(")
	for p.err == nil && !p.acceptPunct(")") {
		switch {
		case p.acceptPunct("..."):
			p.f.Variadic = true
		case p.acceptWord("env"):
			name := p.expect(tTemp, "env parameter").s
			p.defParam(name, ir.ClsL)
		case p.tok.kind == tType:
			agg := p.typesByName[p.tok.s]
			p.advance()
			name := p.expect(tTemp, "parameter name").s
			p.defParam(name, ir.ClsL).Agg = agg
		default:
			cls := p.parseClass()
			name := p.expect(tTemp, "parameter name").s
			p.defParam(name, cls)
		}
		if !p.acceptPunct(",") {
			p.expectPunct(")")
			break
		}
	}
}

func (p *parser) parseBlock() {
	name := p.expect(tLabel, "block label").s
	b := p.block(name)
	// A block joins the function's block list at its label definition, so blocks
	// keep source order regardless of when they were first referenced.
	if !p.appended[b] {
		p.f.Blocks = append(p.f.Blocks, b)
		p.appended[b] = true
		if p.f.Start == nil {
			p.f.Start = b
		}
	}
	for p.err == nil {
		switch {
		case p.tok.kind == tLabel || (p.tok.kind == tPunct && p.tok.s == "}"):
			// A block with no explicit terminator falls through to the next.
			if b.Jmp.Kind == ir.JmpNone && p.tok.kind == tLabel {
				b.Jmp = ir.Jmp{Kind: ir.JmpJmp, To: p.block(p.tok.s)}
			}
			return
		case p.isWord("dbgfile"):
			p.advance()
			p.curPos.File = p.m.File(p.expect(tStr, "file name").s)
		case p.isWord("dbgloc"):
			p.advance()
			p.curPos.Line = uint32(p.expect(tInt, "line").i)
			p.curPos.Col = uint32(p.expect(tInt, "column").i)
		case p.tok.kind == tTemp:
			n := len(b.Instrs)
			p.parseAssign(b)
			p.stampPos(b, n)
		case p.isWord("jmp"), p.isWord("jnz"), p.isWord("ret"), p.isWord("hlt"):
			p.parseTerminator(b)
			return
		case p.tok.kind == tWord:
			n := len(b.Instrs)
			p.parseEffect(b) // result-less instruction (store, call, ...)
			p.stampPos(b, n)
		default:
			p.fail("unexpected %s in block body", p.tok)
			return
		}
	}
}

// stampPos records the current debug position on every instruction added to b
// since index n.
func (p *parser) stampPos(b *ir.Block, n int) {
	for i := n; i < len(b.Instrs); i++ {
		b.Instrs[i].Pos = p.curPos
	}
}

// parseAssign parses "%res =cls op ...".
func (p *parser) parseAssign(b *ir.Block) {
	name := p.expect(tTemp, "result").s
	p.expectPunct("=")
	var cls ir.Cls
	var retAgg *ir.AggType
	if p.tok.kind == tType { // aggregate result (a call): the temporary is a pointer
		cls = ir.ClsL
		retAgg = p.typesByName[p.tok.s]
		p.advance()
	} else {
		cls = p.parseClass()
	}
	op := p.expectWord()
	if op == "phi" {
		p.parsePhi(b, name, cls)
		return
	}
	p.callRetAgg = retAgg
	p.parseInstr(b, name, cls, op)
	p.callRetAgg = nil
}

// parseEffect parses a result-less instruction.
func (p *parser) parseEffect(b *ir.Block) {
	op := p.expectWord()
	p.parseInstr(b, "", ir.ClsW, op)
}

func (p *parser) parsePhi(b *ir.Block, resName string, cls ir.Cls) {
	phi := &ir.Phi{Cls: cls, To: p.defTemp(resName, cls)}
	for p.err == nil {
		blk := p.block(p.expect(tLabel, "phi predecessor").s)
		val := p.parseValue(cls)
		phi.Args = append(phi.Args, val)
		phi.Blocks = append(phi.Blocks, blk)
		if !p.acceptPunct(",") {
			break
		}
	}
	b.Phis = append(b.Phis, phi)
}

func (p *parser) parseInstr(b *ir.Block, resName string, cls ir.Cls, opWord string) {
	// QBE's generic "load" reads a value of the instruction's own class.
	if opWord == "load" {
		p.parseGeneric(b, resName, cls, loadOpForClass(cls))
		return
	}
	if opWord == "tailcall" {
		p.parseCall(b, resName, cls, true)
		return
	}
	if op, ok := ir.OpFromString(opWord); ok {
		if op == ir.OCall {
			p.parseCall(b, resName, cls, false)
			return
		}
		p.parseGeneric(b, resName, cls, op)
		return
	}
	if op, ok := opAlias[opWord]; ok {
		p.parseGeneric(b, resName, cls, op)
		return
	}
	if cmp, argCls, ok := parseCmp(opWord); ok {
		a := p.parseValue(argCls)
		p.expectPunct(",")
		bb := p.parseValue(argCls)
		in := ir.Instr{Op: ir.OCmp, Cls: cls, Cmp: cmp, Args: []ir.Ref{a, bb}}
		if resName != "" {
			in.To = p.defTemp(resName, cls)
		}
		b.Instrs = append(b.Instrs, in)
		return
	}
	p.fail("unknown instruction %q", opWord)
}

func (p *parser) parseGeneric(b *ir.Block, resName string, cls ir.Cls, op ir.Op) {
	in := ir.Instr{Op: op, Cls: cls}
	if resName != "" && op.HasResult() {
		in.To = p.defTemp(resName, cls)
	}
	for i := 0; isValueStart(p.tok); i++ {
		in.Args = append(in.Args, p.parseValue(opArgClass(op, cls, i)))
		if !p.acceptPunct(",") {
			break
		}
	}
	b.Instrs = append(b.Instrs, in)
}

func (p *parser) parseCall(b *ir.Block, resName string, cls ir.Cls, tail bool) {
	callee := p.parseValue(ir.ClsL)
	args := []ir.Ref{callee}
	var aggArgs []*ir.AggType
	anyAgg := false
	p.expectPunct("(")
	for p.err == nil && !p.acceptPunct(")") {
		switch {
		case p.acceptPunct("..."):
			// variadic marker
		case p.acceptWord("env"):
			args = append(args, p.parseValue(ir.ClsL))
			aggArgs = append(aggArgs, nil)
		case p.tok.kind == tType:
			agg := p.typesByName[p.tok.s]
			p.advance()
			args = append(args, p.parseValue(ir.ClsL)) // a pointer to the aggregate
			aggArgs = append(aggArgs, agg)
			anyAgg = true
		default:
			argCls := p.parseClass()
			args = append(args, p.parseValue(argCls))
			aggArgs = append(aggArgs, nil)
		}
		if !p.acceptPunct(",") {
			p.expectPunct(")")
			break
		}
	}
	in := ir.Instr{Op: ir.OCall, Cls: cls, Args: args, RetAgg: p.callRetAgg, Tail: tail}
	if anyAgg {
		in.AggArgs = aggArgs
	}
	if resName != "" {
		in.To = p.defTemp(resName, cls)
	}
	b.Instrs = append(b.Instrs, in)
}

func (p *parser) parseTerminator(b *ir.Block) {
	switch {
	case p.acceptWord("ret"):
		if isValueStart(p.tok) {
			b.Jmp = ir.Jmp{Kind: ir.JmpRet, Arg: p.parseValue(p.f.Retty)}
		} else {
			b.Jmp = ir.Jmp{Kind: ir.JmpRet}
		}
	case p.acceptWord("jmp"):
		b.Jmp = ir.Jmp{Kind: ir.JmpJmp, To: p.block(p.expect(tLabel, "jump target").s)}
	case p.acceptWord("jnz"):
		cond := p.parseValue(ir.ClsW)
		p.expectPunct(",")
		t1 := p.block(p.expect(tLabel, "true target").s)
		p.expectPunct(",")
		t2 := p.block(p.expect(tLabel, "false target").s)
		b.Jmp = ir.Jmp{Kind: ir.JmpJnz, Arg: cond, To: t1, To2: t2}
	case p.acceptWord("hlt"):
		b.Jmp = ir.Jmp{Kind: ir.JmpHlt}
	default:
		p.fail("expected a terminator, got %s", p.tok)
	}
}

// --- values ---------------------------------------------------------------

func (p *parser) parseValue(cls ir.Cls) ir.Ref {
	t := p.tok
	switch t.kind {
	case tTemp:
		p.advance()
		return p.temp(t.s)
	case tInt:
		p.advance()
		return p.f.ConstInt(cls, t.i)
	case tFloat:
		p.advance()
		if t.single {
			return p.f.Single(t.f)
		}
		return p.f.Double(t.f)
	case tGlobal:
		p.advance()
		var off int64
		if p.acceptPunct("+") {
			off = p.expect(tInt, "offset").i
		}
		return p.f.SymClass(t.s, off, cls)
	}
	p.fail("expected a value, got %s", t)
	return ir.R
}

func isValueStart(t token) bool {
	switch t.kind {
	case tTemp, tInt, tFloat, tGlobal:
		return true
	}
	return false
}

// --- per-function symbol tables -------------------------------------------

func (p *parser) defParam(name string, cls ir.Cls) *ir.Temp {
	r := p.f.NewTemp(name, cls)
	p.temps[name] = r
	t := p.f.Temp(r)
	p.f.Params = append(p.f.Params, t)
	return t
}

func (p *parser) defTemp(name string, cls ir.Cls) ir.Ref {
	if r, ok := p.temps[name]; ok {
		p.f.Temp(r).Cls = cls // a forward reference now learns its class
		return r
	}
	r := p.f.NewTemp(name, cls)
	p.temps[name] = r
	return r
}

func (p *parser) temp(name string) ir.Ref {
	if r, ok := p.temps[name]; ok {
		return r
	}
	r := p.f.NewTemp(name, ir.ClsW) // placeholder class until the definition is seen
	p.temps[name] = r
	return r
}

// block returns the block with the given name, creating it detached (not yet in
// the function's block list) on first reference. parseBlock appends it in source
// order when its label is reached.
func (p *parser) block(name string) *ir.Block {
	if b, ok := p.blocks[name]; ok {
		return b
	}
	b := &ir.Block{Name: name}
	p.blocks[name] = b
	return b
}
