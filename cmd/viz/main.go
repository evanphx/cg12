// Command viz renders the cg12 pipeline as a static HTML page: the input C
// program, the IR as it changes across each optimization pass, and the final
// arm64 assembly — one column per stage, left to right.
//
//	viz prog.c            # write the page to stdout
//	viz -o out.html prog.c
package main

import (
	"flag"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

// stage is one column of the visualization.
type stage struct {
	title string // short label (C, IR, asm)
	sub   string // stage detail (frontend, after mem2reg, arm64)
	kind  string // css/highlight kind: c | ir | asm
	text  string
}

func main() {
	out := flag.String("o", "", "output HTML file (default stdout)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: viz [-o out.html] file.c")
		os.Exit(2)
	}
	input := flag.Arg(0)

	src, err := os.ReadFile(input)
	check(err)

	mod, err := cc.Compile(filepath.Base(input), string(src))
	check(err)

	stages := []stage{
		{title: "C", sub: filepath.Base(input), kind: "c", text: string(src)},
		{title: "IR", sub: "frontend", kind: "ir", text: mod.String()},
	}

	// Run the optimization pipeline one pass at a time, capturing the IR after
	// each. It mirrors opt.DefaultPipeline's structure (the clean set and the
	// inliner run to a fixpoint), but each individual pass is its own column;
	// passes that leave the IR unchanged are folded into the next visible stage's
	// label rather than shown as a redundant column.
	last := mod.String()
	var pending []string
	capture := func(name string) {
		pending = append(pending, name)
		now := mod.String()
		if now == last {
			return
		}
		// The last pass in the pending list is the one that changed the IR; any
		// earlier ones ran without effect and are summarized as a count.
		label := pending[len(pending)-1]
		if noop := len(pending) - 1; noop > 0 {
			label += fmt.Sprintf(" (+%d unchanged)", noop)
		}
		stages = append(stages, stage{title: "IR", sub: "after " + label, kind: "ir", text: now})
		last = now
		pending = nil
	}
	runPipeline(mod, capture)

	// The final column is the target assembly, read back out of the compiled
	// object (a clone, so lowering does not disturb the IR we just printed).
	o, err := arm64.CompileToObject(clone(mod))
	check(err)
	stages = append(stages, stage{title: "asm", sub: "arm64", kind: "asm", text: arm64.Disassemble(o)})

	page := render(input, stages)
	if *out == "" {
		fmt.Print(page)
	} else {
		check(os.WriteFile(*out, []byte(page), 0o644))
	}
}

// runPipeline runs the optimizer as opt.DefaultPipeline does — the clean set and
// the inliner iterate to a fixpoint — but reports each individual pass to capture
// so a visualization can show it. Structure and pass order match the real
// pipeline exactly.
func runPipeline(m *ir.Module, capture func(name string)) {
	run := func(name string, p func(*ir.Module) bool) { p(m); capture(name) }

	clean := func() {
		for {
			before := m.String()
			run("fold", funcPass(opt.Fold))
			run("copy", funcPass(opt.Copy))
			run("loadelim", funcPass(opt.LoadElim))
			run("deadalloc", funcPass(opt.DeadAlloc))
			run("gvn", funcPass(opt.GVN))
			run("simplifycfg", funcPass(opt.SimplifyCFG))
			run("dce", funcPass(opt.DCE))
			if m.String() == before {
				return
			}
		}
	}
	inlineFixpoint := func() {
		for {
			before := m.String()
			run("inline", opt.Inline)
			clean()
			if m.String() == before {
				return
			}
		}
	}

	run("mem2reg", funcPass(opt.Mem2Reg))
	clean()
	inlineFixpoint()
	run("unroll", opt.UnrollRecursion)
	inlineFixpoint()
	run("deadfunc", opt.DeadFuncElim)
	run("gcm", funcPass(opt.GCM))
	run("dce", funcPass(opt.DCE))
}

// funcPass lifts a per-function pass to the module, running it over every
// function with a body.
func funcPass(run func(*ir.Func) bool) func(*ir.Module) bool {
	return func(m *ir.Module) bool {
		changed := false
		for _, f := range m.Funcs {
			if f.Start != nil && run(f) {
				changed = true
			}
		}
		return changed
	}
}

// clone deep-copies a module through its binary form, so compiling it (which
// lowers in place) leaves the original untouched.
func clone(m *ir.Module) *ir.Module {
	data, err := m.MarshalBinary()
	check(err)
	c, err := ir.DecodeModule(data)
	check(err)
	return c
}

func check(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "viz: %v\n", err)
		os.Exit(1)
	}
}

// --- rendering -------------------------------------------------------------

var (
	irToken  = regexp.MustCompile(`(%[\w.]+|@[\w.]+|\$[\w.]+|#-?[\w]+|\b-?\d+\b)`)
	asmToken = regexp.MustCompile(`([xw]\d+|[sdq]\d+|wzr|xzr|wsp|sp|#-?\w+|\b-?\d+\b)`)
)

// highlight escapes one raw line and wraps its recognizable tokens in spans. It
// works on the raw text (not pre-escaped) so a token pattern like the #imm form
// can never match the digits inside an HTML entity such as &#34;.
func highlight(kind, raw string) string {
	var re *regexp.Regexp
	var classOf func(string) string
	switch kind {
	case "ir":
		re, classOf = irToken, irClass
	case "asm":
		re, classOf = asmToken, asmClass
	default:
		return html.EscapeString(raw)
	}
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringIndex(raw, -1) {
		b.WriteString(html.EscapeString(raw[last:loc[0]]))
		m := raw[loc[0]:loc[1]]
		b.WriteString(span(classOf(m), html.EscapeString(m)))
		last = loc[1]
	}
	b.WriteString(html.EscapeString(raw[last:]))
	return b.String()
}

func irClass(m string) string {
	switch m[0] {
	case '%':
		return "t-temp"
	case '@':
		return "t-block"
	case '$':
		return "t-sym"
	case '#':
		return "t-num"
	default:
		return "t-num"
	}
}

func asmClass(m string) string {
	switch {
	case m[0] == '#' || (m[0] >= '0' && m[0] <= '9') || m[0] == '-':
		return "t-num"
	default:
		return "t-reg"
	}
}

func span(class, s string) string { return `<span class="` + class + `">` + s + `</span>` }

// codeLines renders the stage body as a gutter-numbered, highlighted block.
func codeLines(kind, text string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString(`<div class="ln">`)
		b.WriteString(highlight(kind, line))
		b.WriteString("</div>")
	}
	return b.String()
}

func render(input string, stages []stage) string {
	var cols strings.Builder
	for i, s := range stages {
		cols.WriteString(fmt.Sprintf(`<section class="stage k-%s">`, s.kind))
		cols.WriteString(`<header><span class="num">` + fmt.Sprint(i+1) + `</span>`)
		cols.WriteString(`<span class="title">` + html.EscapeString(s.title) + `</span>`)
		cols.WriteString(`<span class="sub">` + html.EscapeString(s.sub) + `</span></header>`)
		cols.WriteString(`<div class="code">` + codeLines(s.kind, s.text) + `</div>`)
		cols.WriteString(`</section>`)
	}
	r := strings.NewReplacer(
		"__NAME__", html.EscapeString(filepath.Base(input)),
		"__COUNT__", fmt.Sprint(len(stages)),
		"__COLS__", cols.String(),
	)
	return r.Replace(pageTemplate)
}

const pageTemplate = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>cg12 pipeline — __NAME__</title>
<style>
:root {
  --bg:#0f1117; --panel:#161923; --edge:#242838; --head:#1c2030;
  --fg:#c8cdda; --dim:#7b8398; --accent:#6ea8fe;
  --temp:#9ece6a; --block:#e0af68; --sym:#bb9af7; --num:#7dcfff; --reg:#f7768e;
}
* { box-sizing:border-box; }
html,body { margin:0; height:100%; overflow:hidden; background:var(--bg); color:var(--fg);
  font:13px/1.5 -apple-system,Segoe UI,Roboto,sans-serif; }
.topbar { display:flex; align-items:baseline; gap:12px;
  padding:10px 16px; background:var(--head); border-bottom:1px solid var(--edge); }
.topbar h1 { margin:0; font-size:14px; font-weight:600; }
.topbar .meta { color:var(--dim); font-size:12px; }
.pipeline { display:flex; gap:14px; padding:16px; align-items:flex-start;
  height:calc(100vh - 43px); overflow:auto; }
.stage { flex:0 0 auto; width:46ch; max-width:80vw; background:var(--panel);
  border:1px solid var(--edge); border-radius:8px; overflow:hidden; }
.stage header { position:sticky; top:0; z-index:1; display:flex; align-items:baseline; gap:8px;
  padding:8px 12px; background:var(--head); border-bottom:1px solid var(--edge);
  box-shadow:0 2px 6px rgba(0,0,0,.35); }
.stage .num { display:inline-flex; align-items:center; justify-content:center;
  width:20px; height:20px; border-radius:50%; background:var(--edge); color:var(--dim);
  font-size:11px; font-weight:700; }
.stage .title { font-weight:700; letter-spacing:.02em; }
.stage .sub { color:var(--dim); font-size:12px; }
.k-c header { border-bottom-color:#3b4b6b; } .k-c .title { color:var(--accent); }
.k-asm header { border-bottom-color:#5b3b4b; } .k-asm .title { color:var(--reg); }
.code { counter-reset:l; padding:8px 0; overflow-x:auto;
  font-family:"SF Mono",ui-monospace,Menlo,Consolas,monospace; font-size:12px; }
.ln { counter-increment:l; white-space:pre; padding:0 12px 0 3.2em; position:relative; min-height:1.5em; }
.ln::before { content:counter(l); position:absolute; left:0; width:2.6em; text-align:right;
  color:#454b5e; }
.ln:hover { background:#1b1f2c; }
.t-temp { color:var(--temp); } .t-block { color:var(--block); } .t-sym { color:var(--sym); }
.t-num { color:var(--num); } .t-reg { color:var(--reg); }
</style></head><body>
<div class="topbar"><h1>cg12 pipeline</h1>
<span class="meta">__NAME__ — __COUNT__ stages · C → IR through each optimization → arm64</span></div>
<div class="pipeline">__COLS__</div>
</body></html>
`
