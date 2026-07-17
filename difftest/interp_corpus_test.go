package difftest

import (
	"strconv"
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// The interpreter runs the IL corpus directly, with no backend, assembler, or
// host — a target-independent oracle beside the compiled arm64/QBE arms. The C
// driver in each Case cannot run here, so an invocation table transcribes what
// the driver does: call function fn with these arguments and format the result
// with the C verb the driver printed. Cases whose driver builds memory or reads
// through a pointer (dsum, memory) need the memory model and arrive in Phase B.
type interpCall struct {
	fn   string
	args []interp.Value
	verb string // the C conversion the driver used: "d", "lld", "u", "g"
}

var interpCorpus = map[string]interpCall{
	"add":          {"add", []interp.Value{interp.W(17), interp.W(25)}, "d"},
	"factorial":    {"fact", []interp.Value{interp.W(10)}, "lld"},
	"fib":          {"fib", []interp.Value{interp.W(15)}, "d"},
	"gcd":          {"gcd", []interp.Value{interp.W(1071), interp.W(462)}, "d"},
	"darith":       {"darith", []interp.Value{interp.D(10), interp.D(4), interp.D(3)}, "g"},
	"conversions":  {"conv", []interp.Value{interp.W(3), interp.S(2.9)}, "g"},
	"dtosi-alias":  {"toint", []interp.Value{interp.D(3.9)}, "lld"},
	"compares":     {"cmps", []interp.Value{interp.W(3), interp.W(5)}, "d"},
	"bitops":       {"bits", []interp.Value{interp.W(12), interp.W(10)}, "d"},
	"unsigned-div": {"u", []interp.Value{interp.W(-16 /* 0xfffffff0 */), interp.W(16)}, "u"},
}

func TestCorpusInterp(t *testing.T) {
	for _, c := range corpus {
		call, ok := interpCorpus[c.Name]
		if !ok {
			continue // needs memory (Phase B) — covered by the compiled arms
		}
		t.Run(c.Name, func(t *testing.T) {
			for _, level := range []string{"raw", "opt"} {
				t.Run(level, func(t *testing.T) {
					m, err := parse.Parse(c.IL)
					require.NoError(t, err, "parse")
					if level == "opt" {
						opt.OptimizeModule(m)
					}
					mc, err := interp.New(m)
					require.NoError(t, err, "load")
					v, err := mc.Call(call.fn, call.args...)
					require.NoError(t, err, "call")
					require.Equal(t, c.Want, cFormat(call.verb, v)+"\n",
						"interp %s(%s) formatted %%%s", call.fn, call.verb, call.verb)
				})
			}
		})
	}
}

// cFormat renders a result value the way the C driver's printf verb would — a
// small C-compatible subset (the seed of the printf intrinsic in Phase C), not
// Go's fmt, whose %g differs from C's.
func cFormat(verb string, v interp.Value) string {
	switch verb {
	case "d", "lld":
		return strconv.FormatInt(v.I64(), 10)
	case "u", "llu":
		return strconv.FormatUint(v.U64(), 10)
	case "g":
		// C %g: up to 6 significant digits, trailing zeros stripped.
		return strconv.FormatFloat(v.F64(), 'g', 6, 64)
	}
	panic("interp corpus: unhandled C verb %" + verb)
}
