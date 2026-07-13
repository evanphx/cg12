package parse_test

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseComments(t *testing.T) {
	m, err := parse.Parse(`
# leading comment
export function w $f(w %a) { # trailing comment
@start
	ret %a   # done
}
`)
	require.NoError(t, err)
	require.Len(t, m.Funcs, 1)
	assert.Equal(t, "f", m.Funcs[0].Name)
	assert.True(t, m.Funcs[0].Linkage.Export)
}

func TestParseStringAndFloatData(t *testing.T) {
	m, err := parse.Parse(`
data $s = { b "hi\n", b 0 }
data $fs = { d d_1.5 d_2.5, s s_3.5 }
`)
	require.NoError(t, err)
	require.Len(t, m.Data, 2)
	assert.Equal(t, "hi\n", m.Data[0].Items[0].Str) // escapes are decoded to bytes
	assert.Equal(t, []float64{1.5, 2.5}, m.Data[1].Items[0].Flts)
	assert.Equal(t, []float64{3.5}, m.Data[1].Items[1].Flts)
}

func TestParseLinkageThreadSection(t *testing.T) {
	m, err := parse.Parse(`
thread data $tls = { l 0 }
section ".weird" function w $g() {
@start
	ret 0
}
`)
	require.NoError(t, err)
	assert.True(t, m.Data[0].Linkage.Thread)
	assert.Equal(t, ".weird", m.Funcs[0].Linkage.Section)
}

func TestParseHexAndNegative(t *testing.T) {
	m, err := parse.Parse(`
function l $c() {
@start
	%x =l add 0xff, -5
	ret %x
}
`)
	require.NoError(t, err)
	// The constants 255 and -5 were interned.
	var have255, haveNeg bool
	for _, c := range m.Funcs[0].Consts {
		if c.Int == 255 {
			have255 = true
		}
		if c.Int == -5 {
			haveNeg = true
		}
	}
	assert.True(t, have255 && haveNeg)
}

func TestParseComparePredicates(t *testing.T) {
	// Exercises int and float compare mnemonic decoding.
	src := `
function w $c(w %a, w %b, d %x, d %y) {
@start
	%1 =w cultw %a, %b
	%2 =w cugew %a, %b
	%3 =w cnew %a, %b
	%4 =w cltd %x, %y
	%5 =w cged %x, %y
	%6 =w cod %x, %y
	%7 =w cuod %x, %y
	ret %1
}
`
	m, err := parse.Parse(src)
	require.NoError(t, err)
	var preds []ir.Cmp
	for _, in := range m.Funcs[0].Blocks[0].Instrs {
		if in.Op == ir.OCmp {
			preds = append(preds, in.Cmp)
		}
	}
	assert.Equal(t, []ir.Cmp{ir.CmpUlt, ir.CmpUge, ir.CmpNe, ir.CmpFlt, ir.CmpFge, ir.CmpFo, ir.CmpFuo}, preds)
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unterminated string": `data $s = { b "oops`,
		"unterminated type":   `type :t = { w`,
		"unknown instruction": "function w $f() {\n@start\n\t%x =w bogus 1\n\tret %x\n}",
		"missing value":       "function w $f() {\n@start\n\tret ,\n}",
		"bad top level":       "@nonsense",
		"bad character":       "function w $f() {\n@start\n\t%x =w add !\n}",
	}
	for name, src := range cases {
		_, err := parse.Parse(src)
		assert.Errorf(t, err, "expected error for %s", name)
	}
}

func TestParseStoreWidths(t *testing.T) {
	m, err := parse.Parse(`
function $f(l %p) {
@start
	storeb 1, %p
	storeh 2, %p
	storew 3, %p
	storel 4, %p
	stores s_1.5, %p
	stored d_2.5, %p
	ret
}
`)
	require.NoError(t, err)
	assert.Len(t, m.Funcs[0].Blocks[0].Instrs, 6)
}

func TestParseFloatExponent(t *testing.T) {
	m, err := parse.Parse(`
function d $f() {
@start
	ret d_1.5e3
}
`)
	require.NoError(t, err)
	arg := m.Funcs[0].Blocks[0].Jmp.Arg
	assert.Equal(t, 1500.0, m.Funcs[0].Consts[arg.ID].Flt)
}

func TestParseTypeErrors(t *testing.T) {
	cases := map[string]string{
		"bad param type": "function w $f(z %x) {\n@start\n\tret 0\n}",
		"bad data elem":  "data $d = { q 1 }",
		"bad field type": "type :t = { q }",
	}
	for name, src := range cases {
		_, err := parse.Parse(src)
		assert.Errorf(t, err, "expected error for %s", name)
	}
}

func TestParseConversionAliases(t *testing.T) {
	// QBE's width-specific conversion mnemonics map onto cg12's merged opcodes.
	m, err := parse.Parse(`
function d $c(d %x, w %n) {
@start
	%a =l dtosi %x
	%b =w dtoui %x
	%c =d swtof %n
	%e =d uwtof %n
	ret %c
}
`)
	require.NoError(t, err)
	ops := []ir.Op{}
	for _, in := range m.Funcs[0].Blocks[0].Instrs {
		ops = append(ops, in.Op)
	}
	assert.Equal(t, []ir.Op{ir.OStosi, ir.OStoui, ir.OSltof, ir.OUltof}, ops)
}

func TestParseFallThrough(t *testing.T) {
	// A block with no terminator falls through to the next.
	m, err := parse.Parse(`
function w $f(w %n) {
@start
	%x =w add %n, 1
@next
	ret %x
}
`)
	require.NoError(t, err)
	start := m.Funcs[0].Blocks[0]
	next := m.Funcs[0].Blocks[1]
	assert.Equal(t, ir.JmpJmp, start.Jmp.Kind)
	assert.Same(t, next, start.Jmp.To)
}

func TestParseLoadForms(t *testing.T) {
	m, err := parse.Parse(`
function w $g(l %p) {
@start
	%a =w loadw %p
	%b =w load %p
	%c =l load %p
	%d =d load %p
	ret %a
}
`)
	require.NoError(t, err)
	var ops []ir.Op
	for _, in := range m.Funcs[0].Blocks[0].Instrs {
		ops = append(ops, in.Op)
	}
	assert.Equal(t, []ir.Op{ir.OLoaduw, ir.OLoaduw, ir.OLoadl, ir.OLoadd}, ops)
}

func TestParseEmptyModule(t *testing.T) {
	m, err := parse.Parse("   # just a comment\n")
	require.NoError(t, err)
	assert.Empty(t, m.Funcs)
	assert.Empty(t, m.Data)
}

func TestParseVariadicAndEnv(t *testing.T) {
	// env parameters and variadic markers are accepted.
	m, err := parse.Parse(`
function w $printf(l %fmt, ...) {
@start
	ret 0
}
function w $caller(env %e, l %fmt) {
@start
	%r =w call $printf(l %fmt, ..., w 1)
	ret %r
}
`)
	require.NoError(t, err)
	require.Len(t, m.Funcs, 2)
	assert.Equal(t, "fmt", m.Funcs[0].Params[0].Name)
}

func TestTokenStringInErrors(t *testing.T) {
	// The error message should name the offending token forms.
	_, err := parse.Parse("function w $f() {\n@start\n\t%x =w add $g, :t\n}")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "line"))
}
