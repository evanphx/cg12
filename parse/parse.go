// Package parse reads QBE-compatible textual IL into cg12's ir structures. It is
// the inverse of ir's textual printer, enabling round-tripping and differential
// testing against QBE's corpus.
package parse

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
)

// Parse reads a whole module of textual IL.
func Parse(src string) (*ir.Module, error) {
	p := &parser{lex: newLexer(src), typesByName: map[string]*ir.AggType{}}
	p.advance()
	m := p.parseModule()
	if p.err != nil {
		return nil, p.err
	}
	return m, nil
}

type parser struct {
	lex *lexer
	tok token
	err error

	typesByName map[string]*ir.AggType
	m           *ir.Module // the module under construction (for the file table)

	// per-function state
	f          *ir.Func
	temps      map[string]ir.Ref
	blocks     map[string]*ir.Block
	appended   map[*ir.Block]bool
	callRetAgg *ir.AggType // aggregate result type of the call currently parsing
	curPos     ir.SrcPos   // current debug position, set by dbgfile/dbgloc
}

func (p *parser) advance() {
	if p.err != nil {
		return
	}
	p.tok, p.err = p.lex.next()
}

func (p *parser) fail(format string, a ...any) {
	if p.err == nil {
		p.err = fmt.Errorf("line %d: %s", p.tok.line, fmt.Sprintf(format, a...))
	}
}

// isWord reports whether the current token is the given bare word.
func (p *parser) isWord(w string) bool { return p.tok.kind == tWord && p.tok.s == w }

// acceptWord consumes the current token if it is the given word.
func (p *parser) acceptWord(w string) bool {
	if p.isWord(w) {
		p.advance()
		return true
	}
	return false
}

func (p *parser) acceptPunct(s string) bool {
	if p.tok.kind == tPunct && p.tok.s == s {
		p.advance()
		return true
	}
	return false
}

func (p *parser) expectPunct(s string) {
	if !p.acceptPunct(s) {
		p.fail("expected %q, got %s", s, p.tok)
	}
}

func (p *parser) expectWord() string {
	if p.tok.kind != tWord {
		p.fail("expected a keyword, got %s", p.tok)
		return ""
	}
	w := p.tok.s
	p.advance()
	return w
}

func (p *parser) expect(kind tokKind, what string) token {
	if p.tok.kind != kind {
		p.fail("expected %s, got %s", what, p.tok)
		return token{}
	}
	t := p.tok
	p.advance()
	return t
}

// --- module ---------------------------------------------------------------

func (p *parser) parseModule() *ir.Module {
	m := ir.NewModule()
	p.m = m
	for p.err == nil && p.tok.kind != tEOF {
		if p.isWord("type") {
			p.parseType(m)
			continue
		}
		lk := p.parseLinkage()
		switch {
		case p.isWord("function"):
			p.parseFunc(m, lk)
		case p.isWord("data"):
			p.parseData(m, lk)
		default:
			p.fail("expected 'function', 'data', or 'type', got %s", p.tok)
		}
	}
	return m
}

func (p *parser) parseLinkage() ir.Linkage {
	var lk ir.Linkage
	for p.err == nil {
		switch {
		case p.acceptWord("export"):
			lk.Export = true
		case p.acceptWord("thread"):
			lk.Thread = true
		case p.acceptWord("section"):
			lk.Section = p.expect(tStr, "section name").s
			if p.tok.kind == tStr {
				lk.SecArgs = p.tok.s
				p.advance()
			}
		default:
			return lk
		}
	}
	return lk
}

// --- types ----------------------------------------------------------------

func (p *parser) parseType(m *ir.Module) {
	p.expectWord() // "type"
	name := p.expect(tType, "type name").s
	p.expectPunct("=")
	t := &ir.AggType{Name: name}
	if p.acceptWord("align") {
		t.Align = int(p.expect(tInt, "alignment").i)
	}
	p.expectPunct("{")
	switch {
	case p.tok.kind == tInt: // opaque: { size }
		t.Opaque = true
		t.Size = int(p.tok.i)
		p.advance()
		p.expectPunct("}")
	case p.tok.kind == tPunct && p.tok.s == "{": // union: { {case} {case} ... }
		t.Union = true
		for p.err == nil && p.tok.kind == tPunct && p.tok.s == "{" {
			p.advance() // consume the case's opening brace
			t.Cases = append(t.Cases, p.parseFields())
		}
		p.expectPunct("}")
	default:
		t.Fields = p.parseFields()
	}
	m.Types = append(m.Types, t)
	p.typesByName[name] = t
}

// parseFields parses a comma-separated list of aggregate fields up to and
// including the closing brace.
func (p *parser) parseFields() []ir.Field {
	var fields []ir.Field
	for p.err == nil && !p.acceptPunct("}") {
		var fld ir.Field
		if p.tok.kind == tType {
			fld.Type = p.typesByName[p.tok.s]
			p.advance()
		} else {
			fld.Sub = p.parseSub()
		}
		if p.tok.kind == tInt {
			fld.Count = int(p.tok.i)
			p.advance()
		}
		fields = append(fields, fld)
		if !p.acceptPunct(",") {
			p.expectPunct("}")
			break
		}
	}
	return fields
}

// --- data -----------------------------------------------------------------

func (p *parser) parseData(m *ir.Module, lk ir.Linkage) {
	p.expectWord() // "data"
	name := p.expect(tGlobal, "data name").s
	p.expectPunct("=")
	d := &ir.Data{Name: name, Linkage: lk}
	if p.acceptWord("align") {
		d.Align = int(p.expect(tInt, "alignment").i)
	}
	p.expectPunct("{")
	curSub := ir.SubW
	for p.err == nil && !p.acceptPunct("}") {
		switch {
		case p.acceptPunct(","):
			// separator
		case p.acceptWord("z"):
			d.Items = append(d.Items, ir.DataItem{Zero: int(p.expect(tInt, "zero count").i)})
		case p.tok.kind == tWord && isDataElem(p.tok.s):
			curSub = p.parseSub()
		case p.tok.kind == tInt:
			p.dataInt(d, curSub, p.tok.i)
			p.advance()
		case p.tok.kind == tFloat:
			p.dataFloat(d, curSub, p.tok.f)
			p.advance()
		case p.tok.kind == tStr:
			d.Items = append(d.Items, ir.DataItem{Sub: curSub, Str: p.tok.s})
			p.advance()
		case p.tok.kind == tGlobal:
			it := ir.DataItem{Sub: curSub, Sym: p.tok.s}
			p.advance()
			if p.acceptPunct("+") {
				it.Off = p.expect(tInt, "offset").i
			}
			d.Items = append(d.Items, it)
		default:
			p.fail("unexpected %s in data", p.tok)
		}
	}
	m.Data = append(m.Data, d)
}

// dataInt appends an integer, merging into the previous item when it is a run of
// integers of the same element type.
func (p *parser) dataInt(d *ir.Data, sub ir.SubCls, v int64) {
	if n := len(d.Items); n > 0 {
		last := &d.Items[n-1]
		if last.Sub == sub && last.Zero == 0 && last.Str == "" && last.Sym == "" && len(last.Flts) == 0 {
			last.Ints = append(last.Ints, v)
			return
		}
	}
	d.Items = append(d.Items, ir.DataItem{Sub: sub, Ints: []int64{v}})
}

func (p *parser) dataFloat(d *ir.Data, sub ir.SubCls, v float64) {
	if n := len(d.Items); n > 0 {
		last := &d.Items[n-1]
		if last.Sub == sub && last.Zero == 0 && last.Str == "" && last.Sym == "" && len(last.Ints) == 0 {
			last.Flts = append(last.Flts, v)
			return
		}
	}
	d.Items = append(d.Items, ir.DataItem{Sub: sub, Flts: []float64{v}})
}
