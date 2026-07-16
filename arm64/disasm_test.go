package arm64_test

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// disasmCorpus is C chosen to make the backend reach for the breadth of the
// instruction set: floating point of both widths, aggregates by value, globals
// and calls, addressing folds, shifts and rotates, both divisions, sign- and
// zero-extending sub-word loads and stores, a jump table, a stacked-argument
// call, a conditional select, and the flag-setting compares. Thread-locals are
// not here: our C frontend has no _Thread_local, so those come from an IR-built
// module instead.
const disasmCorpus = `
struct P { double x, y; };
struct Big { long a, b, c, d, e; };
extern int g_int;
extern struct Big g_big;
extern int helper(int);

double dist(struct P a, struct P b) {
  double dx = a.x - b.x, dy = a.y - b.y;
  return dx*dx + dy*dy;
}
int sum(int *p, int n) { int s = 0; for (int i = 0; i < n; i++) s += p[i]; return s; }
unsigned rot(unsigned v) { return (v << 7) | (v >> 25); }
unsigned rotv(unsigned v, int k) { return (v << k) | (v >> (32 - k)); }
long idx(long *p, int i) { return p[i] + p[i+1]; }
int cls(int a, int b) { if (a > b) return a & 0xff; return b | 0xf0f0f0f0; }
float f2(float a, int b) { return a * (float)b; }
short sh(char *p) { return (short)p[0] + (short)p[1]; }
int sw(int x) { switch (x) { case 1: return 10; case 2: return 20; case 3: return 30;
  case 4: return 40; case 5: return 50; case 6: return 60; default: return 0; } }
long many(long a, long b, long c, long d, long e, long f, long g, long h, long i) {
  return a+b+c+d+e+f+g+h+i; }
int cmpz(int x) { return x == 0; }
long shifts(long x) { return (x << 3) + (x >> 5) + (x / 7) + (x % 3); }

int glob(int x) { g_int = x; return g_int + helper(x); }          /* adrp/add, bl */
struct Big copy(void) { return g_big; }                            /* a wide copy */
unsigned udivmod(unsigned a, unsigned b) { return a / b + a % b; } /* udiv, msub */
int fcmps(float a, float b, double c, double d) {                  /* fcmp, both widths */
  return (a < b) + (c > d);
}
double fconv(float a, int b, unsigned c, long d) {                 /* the convert matrix */
  return (double)a + (double)b + (double)c + (double)d;
}
int ftoi(double a, float b) { return (int)a + (unsigned)b; }
double fdivneg(double a, double b) { return -(a / b); }
int sel(int a, int b, int c) { return c ? a : b; }                 /* csel */
void subword(char *c, short *s, unsigned char *uc, unsigned short *us) {
  *c = *c + 1; *s = *s + 1; *uc = *uc + 1; *us = *us + 1;          /* the sub-word loads/stores */
}
long widen(char *c, short *s, int *i) { return *c + *s + *i; }     /* sign-extending loads */
int neg(int a, long b) { return -a + (int)~b; }                    /* neg, mvn */
int bits(int a, int b) { return (a & ~b) | (a ^ b); }              /* bic, eor */
long big(long x) { return x + 0x123456789L; }                      /* movz/movk chain */
int (*fp(void))(int) { return helper; }                            /* an indirect call target */
int callp(int (*f)(int), int x) { return f(x); }                   /* blr */
`

// The disassembler is checked here against everything the backend actually
// emits, not just the hand-listed encodings the a64 tests cover. The property is
// total: feeding Disasm's output back to the reference assembler must reproduce
// the original bytes exactly. A word we decode wrongly re-assembles to something
// else; a word we do not decode at all comes back as `.inst 0x...`, which
// re-assembles to itself -- so a pass means every instruction is either decoded
// correctly or honestly reported, with no third outcome where a wrong-but-
// plausible mnemonic slips through.
func TestDisasmRoundTripsThroughTheAssembler(t *testing.T) {
	as, objcopy := disasmTools(t)

	for name, text := range disasmText(t) {
		t.Run(name, func(t *testing.T) {
			lines := disasmLines(text)

			dir := t.TempDir()
			src := filepath.Join(dir, "back.s")
			obj := filepath.Join(dir, "back.o")
			bin := filepath.Join(dir, "back.bin")
			require.NoError(t, os.WriteFile(src, []byte(".text\n"+strings.Join(lines, "\n")+"\n"), 0o644))

			out, err := exec.Command(as, "-o", obj, src).CombinedOutput()
			require.NoErrorf(t, err, "our disassembly does not assemble: %s", out)
			out, err = exec.Command(objcopy, "-O", "binary", "--only-section=.text", obj, bin).CombinedOutput()
			require.NoErrorf(t, err, "objcopy failed: %s", out)
			got, err := os.ReadFile(bin)
			require.NoError(t, err)

			require.Len(t, got, len(text), "one word back out for every word in")
			for i := 0; i+4 <= len(text); i += 4 {
				want := binary.LittleEndian.Uint32(text[i:])
				assert.Equalf(t, want, binary.LittleEndian.Uint32(got[i:]),
					"word %d read as %q", i/4, lines[i/4])
			}
		})
	}
}

// Nothing the backend emits should read as `.inst`: that is the decoder saying it
// does not know an instruction we ourselves encode, which would leave a hole in
// the tests that read our machine code.
func TestDisasmKnowsEverythingTheBackendEmits(t *testing.T) {
	for name, text := range disasmText(t) {
		t.Run(name, func(t *testing.T) {
			for i, line := range disasmLines(text) {
				assert.Falsef(t, strings.HasPrefix(line, ".inst"),
					"word %d is emitted but not decoded: %s", i, line)
			}
		})
	}
}

// disasmText compiles the corpora and returns their .text bytes. The C corpus is
// the broad one; the TLS module covers the thread-pointer read, which our C
// frontend cannot ask for.
func disasmText(t *testing.T) map[string][]byte {
	t.Helper()
	m, err := cc.Compile("corpus.c", disasmCorpus)
	require.NoError(t, err)
	broad, err := arm64.CompileToObject(m)
	require.NoError(t, err)
	require.Greater(t, len(broad.Text)/4, 100, "the corpus should reach a good part of the encoder")

	tls, err := arm64.CompileToObject(bumpModule())
	require.NoError(t, err)

	return map[string][]byte{"c-corpus": broad.Text, "thread-local": tls.Text}
}

func disasmLines(text []byte) []string {
	var lines []string
	for i := 0; i+4 <= len(text); i += 4 {
		lines = append(lines, a64.Disasm(binary.LittleEndian.Uint32(text[i:])))
	}
	return lines
}

// disasmTools locates an AArch64 assembler and objcopy, or skips.
func disasmTools(t *testing.T) (as, objcopy string) {
	t.Helper()
	find := func(names ...string) (string, bool) {
		for _, n := range names {
			if p, err := exec.LookPath(n); err == nil {
				return p, true
			}
		}
		return "", false
	}
	as, ok := find("aarch64-linux-gnu-as")
	if !ok {
		if runtime.GOARCH != "arm64" {
			t.Skip("no AArch64 assembler available")
		}
		if as, ok = find("as"); !ok {
			t.Skip("no assembler available")
		}
	}
	objcopy, ok = find("aarch64-linux-gnu-objcopy")
	if !ok {
		if runtime.GOARCH != "arm64" {
			t.Skip("no AArch64 objcopy available")
		}
		if objcopy, ok = find("objcopy"); !ok {
			t.Skip("no objcopy available")
		}
	}
	return as, objcopy
}

// A call, a branch, and a global's address must come back with the symbol named,
// not the zero the relocation leaves in the instruction. This is what the tests
// that read assembly need in order to check what was selected -- "bl sink" says
// the call went where it should; ".+0" says nothing.
func TestDisassembleNamesRelocatedSymbols(t *testing.T) {
	m, err := cc.Compile("named.c", `
extern int helper(int);
extern int g_int;
int caller(int x) { g_int = x; return helper(x); }
`)
	require.NoError(t, err)
	o, err := arm64.CompileToObject(m)
	require.NoError(t, err)

	asm := arm64.Disassemble(o)
	assert.Contains(t, asm, "caller:\n", "a symbol in .text starts a label")
	assert.Contains(t, asm, "\tbl helper\n", "the call names its target")
	assert.Contains(t, asm, "adrp x", "the global's page")
	assert.Contains(t, asm, "#:lo12:g_int", "and its offset within that page")
	assert.NotContains(t, asm, "bl .", "no relocated branch should read as an offset")
}

// A thread-local's access is a relocation too, and reads as the source would
// write it: the offset from the thread pointer, named rather than left as zero.
func TestDisassembleNamesThreadLocalRelocs(t *testing.T) {
	o, err := arm64.CompileToObject(bumpModule())
	require.NoError(t, err)

	asm := arm64.Disassemble(o)
	assert.Contains(t, asm, "mrs x", "the thread pointer is read")
	assert.Contains(t, asm, "#:tprel_hi12:tls_counter", "the high half of the offset")
	assert.Contains(t, asm, "#:tprel_lo12_nc:tls_counter", "and the low half")
}

// The symbolic forms above are claims about assembler syntax -- that
// "#:tprel_hi12:x" asks for the relocation we think it does. Self-consistency
// cannot check that, so this hands our disassembly to the reference assembler
// and requires that the relocations it produces are exactly the ones our object
// carries: same offset, same type, same symbol, same addend. If a form meant
// something else, the type would differ.
func TestDisassembleRelocsMeanWhatTheySay(t *testing.T) {
	as, _ := disasmTools(t)

	for name, o := range disasmObjects(t) {
		t.Run(name, func(t *testing.T) {
			require.NotEmpty(t, o.Relocs, "there is nothing to check without relocations")

			dir := t.TempDir()
			src := filepath.Join(dir, "sym.s")
			out := filepath.Join(dir, "sym.o")
			require.NoError(t, os.WriteFile(src, []byte(".text\n"+arm64.Disassemble(o)), 0o644))
			msg, err := exec.Command(as, "-o", out, src).CombinedOutput()
			require.NoErrorf(t, err, "our disassembly does not assemble: %s", msg)

			assert.Equal(t, relocKeys(o.Relocs), asRelocKeys(t, out))
		})
	}
}

// relocKeys renders relocations as comparable text, ordered so two lists of the
// same relocations compare equal whatever order they were recorded in.
func relocKeys(relocs []obj.Reloc) []string {
	keys := make([]string, 0, len(relocs))
	for _, r := range relocs {
		keys = append(keys, fmt.Sprintf("%#x type=%d %s%+d", r.Offset, r.Type, r.Sym, r.Addend))
	}
	sort.Strings(keys)
	return keys
}

// asRelocKeys reads .rela.text out of an object the reference assembler wrote.
func asRelocKeys(t *testing.T, path string) []string {
	t.Helper()
	f, err := elf.Open(path)
	require.NoError(t, err)
	defer f.Close()

	syms, err := f.Symbols()
	require.NoError(t, err)
	sec := f.Section(".rela.text")
	require.NotNil(t, sec, "the assembler recorded no relocations at all")
	data, err := sec.Data()
	require.NoError(t, err)

	var relocs []obj.Reloc
	for i := 0; i+24 <= len(data); i += 24 {
		info := f.ByteOrder.Uint64(data[i+8:])
		name := ""
		if idx := info >> 32; idx > 0 && int(idx) <= len(syms) {
			name = syms[idx-1].Name
		}
		relocs = append(relocs, obj.Reloc{
			Offset: f.ByteOrder.Uint64(data[i:]),
			Type:   uint32(info),
			Sym:    name,
			Addend: int64(f.ByteOrder.Uint64(data[i+16:])),
		})
	}
	return relocKeys(relocs)
}

// disasmObjects compiles the corpora to objects, relocations and all.
func disasmObjects(t *testing.T) map[string]*obj.Object {
	t.Helper()
	m, err := cc.Compile("corpus.c", disasmCorpus)
	require.NoError(t, err)
	broad, err := arm64.CompileToObject(m)
	require.NoError(t, err)

	tls, err := arm64.CompileToObject(bumpModule())
	require.NoError(t, err)

	return map[string]*obj.Object{"c-corpus": broad, "thread-local": tls}
}
