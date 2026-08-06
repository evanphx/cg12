// splitprobe finds out why splitting the optimiser pipeline at the Option B/C
// boundary changes what the compiler emits.
//
// CCWORK_REPORT.md's Option C stage 1 measured that running `mem2reg`+`clean` as
// one opt.Run and the remainder as a second moves three functions --
// internal/strconv.trimZeros, runtime.decoderune and syscall.Write -- and
// attributed it to the changeLog. A byte-identity guard on a memoised compile
// cannot be turned on while the boundary itself moves output, so stage 2 has to
// know which of the two candidate causes it actually is.
//
// The split has two independent effects and this probe separates them:
//
//   - the second half gets a fresh changeLog, so passes re-run on functions the
//     unsplit pipeline had already recorded as converged;
//   - if the second half also rebuilds its passes, per-function budgets that a
//     pass instance carries (jump threading's thread and growth caps, the
//     inliner's growth cap) start again from zero.
//
// Four arms, a 2x2 over those two: -arm unsplit, split-rebuilt (stage 1's
// shape), split-shared (one pipeline sliced, two Run calls) and split-session
// (one pipeline sliced, one opt.Session). Each writes a digest per function; the
// arm that differs from unsplit names the cause.
package main

import (
	"bufio"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"

	_ "github.com/evanphx/cg12/arm64" // registers opt.NoSplitFrameBudgetFor
)

// sharedJT, when non-nil, is a jump-threading pass instance the rebuilt halves
// both use, so the -arm split-rebuilt-sharedjt run differs from split-rebuilt in
// exactly one thing: whether the jump-thread budget survives the split.
var sharedJT opt.Pass

func jumpThread() opt.Pass {
	if sharedJT != nil {
		return sharedJT
	}
	return opt.JumpThreadPass()
}

// rebuiltStage reconstructs the whole-module half of DefaultPipeline from fresh
// pass objects, which is what cmd/depsets does and therefore what stage 1
// measured.
func rebuiltStage() []opt.Pass {
	clean := func() opt.Pass {
		return opt.Fixpoint("clean",
			opt.FuncPass("fold", opt.Fold),
			opt.FuncPass("copy", opt.Copy),
			opt.FuncPass("loadelim", opt.LoadElim),
			opt.FuncPass("deadalloc", opt.DeadAlloc),
			opt.FuncPass("gvn", opt.GVN),
			jumpThread(),
			opt.FuncPass("simplifycfg", opt.SimplifyCFG),
			opt.FuncPass("dce", opt.DCE),
		)
	}
	c := clean()
	inline := opt.InlinePass()
	return []opt.Pass{
		opt.Fixpoint("inline-fixpoint", inline, c),
		opt.FuncPass("mem2reg", opt.Mem2Reg),
		c,
		opt.ModulePass("unroll", opt.UnrollRecursion),
		opt.Fixpoint("inline-fixpoint", inline, c),
		opt.FuncPass("constantp", opt.ResolveConstantP),
		opt.FuncPass("ifconvert", opt.IfConvert),
		c,
		opt.FuncPass("tailmerge", opt.TailMerge),
		opt.FuncPass("simplifycfg", opt.SimplifyCFG),
		opt.FuncPass("dce", opt.DCE),
		opt.ModulePass("deadfunc", opt.DeadFuncElim),
		opt.FuncPass("gcm", opt.GCM),
		opt.FuncPass("dce", opt.DCE),
		opt.ModulePass("inline-nosplit", opt.InlineIntoNoSplitCallers),
	}
}

func rebuiltPrefix() []opt.Pass {
	return []opt.Pass{opt.FuncPass("mem2reg", opt.Mem2Reg), opt.Fixpoint("clean",
		opt.FuncPass("fold", opt.Fold),
		opt.FuncPass("copy", opt.Copy),
		opt.FuncPass("loadelim", opt.LoadElim),
		opt.FuncPass("deadalloc", opt.DeadAlloc),
		opt.FuncPass("gvn", opt.GVN),
		jumpThread(),
		opt.FuncPass("simplifycfg", opt.SimplifyCFG),
		opt.FuncPass("dce", opt.DCE),
	)}
}

func digest(f *ir.Func) string {
	sum := sha256.Sum256([]byte(f.String()))
	return fmt.Sprintf("%x", sum[:12])
}

func writeDigests(path string, m *ir.Module) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	defer w.Flush()
	names := make([]string, 0, len(m.Funcs))
	byName := make(map[string]*ir.Func, len(m.Funcs))
	for _, f := range m.Funcs {
		names = append(names, f.Name)
		byName[f.Name] = f
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "%s %s\n", digest(byName[name]), name)
	}
	return nil
}

func main() {
	arm := flag.String("arm", "unsplit", "unsplit | split-rebuilt | split-shared | split-session")
	out := flag.String("out", "", "write per-function digests here")
	flag.Parse()
	if flag.NArg() != 1 || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: splitprobe -arm ARM -out digests program.go")
		os.Exit(2)
	}
	src, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	start := time.Now()
	m, err := goc.CompileExecutableFor(goc.TargetARM64, filepath.Base(flag.Arg(0)), src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "frontend:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "splitprobe %s: front end %.2fs, %d funcs\n", *arm, time.Since(start).Seconds(), len(m.Funcs))

	optStart := time.Now()
	switch *arm {
	case "unsplit":
		opt.Run(m, opt.DefaultPipeline())
	case "split-rebuilt":
		opt.Run(m, rebuiltPrefix())
		opt.Run(m, rebuiltStage())
	case "split-rebuilt-sharedjt":
		sharedJT = opt.JumpThreadPass()
		opt.Run(m, rebuiltPrefix())
		opt.Run(m, rebuiltStage())
	case "timed":
		// Where the whole-module stage's time actually goes, per top-level
		// pipeline entry. `clean` is one shared instance appearing at four
		// positions, so its rows are separate readings of the same object.
		p := opt.DefaultPipeline()
		s := opt.NewSession()
		for i, pass := range p {
			passStart := time.Now()
			s.Run(m, p[i:i+1])
			half := "prefix"
			if i >= opt.PerFunctionPrefixLen {
				half = "stage"
			}
			fmt.Fprintf(os.Stderr, "  %-6s %2d %-16s %7.3fs  %d funcs\n",
				half, i, pass.Name(), time.Since(passStart).Seconds(), len(m.Funcs))
		}
	case "split-shared":
		p := opt.DefaultPipeline()
		opt.Run(m, p[:opt.PerFunctionPrefixLen])
		opt.Run(m, p[opt.PerFunctionPrefixLen:])
	case "split-session":
		p := opt.DefaultPipeline()
		s := opt.NewSession()
		s.Run(m, p[:opt.PerFunctionPrefixLen])
		s.Run(m, p[opt.PerFunctionPrefixLen:])
	default:
		fmt.Fprintln(os.Stderr, "unknown arm", *arm)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "  optimiser %.2fs, %d funcs survive\n", time.Since(optStart).Seconds(), len(m.Funcs))
	if err := writeDigests(*out, m); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
