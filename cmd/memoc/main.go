// memoc compiles a Go program the way goc does, with BUILD_CACHE.md's Option C
// memo in front of the whole-module optimiser stage and the back end.
//
// It exists to answer three questions with numbers rather than arithmetic: what
// a memoised compile actually saves, what it hits, and -- the one that decides
// whether any of it counts -- whether it emits exactly what an unmemoised compile
// emits. The last is not a side check: `-no-memo` runs the identical code path
// with the store ignored, and both arms write the object and the translated
// assembly, so the two can be compared byte for byte.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/memo"
	"github.com/evanphx/cg12/opt"
)

type timings struct {
	Program   string  `json:"program"`
	Arm       string  `json:"arm"`
	FrontEnd  float64 `json:"frontend"`
	Prefix    float64 `json:"prefix"`
	Digest    float64 `json:"digest"`
	Lookup    float64 `json:"lookup"`
	Stage     float64 `json:"stage"`
	Record    float64 `json:"record"`
	Backend   float64 `json:"backend"`
	StoreIO   float64 `json:"storeio"`
	Total     float64 `json:"total"`
	Funcs     int     `json:"funcs"`
	Hits      int     `json:"hits"`
	Misses    int     `json:"misses"`
	Excluded  int     `json:"excluded"`
	DraggedIn int     `json:"draggedin"`
	Survivors int     `json:"survivors"`
	BackHits  int     `json:"backhits"`
	Static    int     `json:"static"`
	Whole     bool    `json:"whole_module"`
	StoreMB   float64 `json:"store_mb"`
	MemoMB    float64 `json:"memo_mb"`
	PeakMB    float64 `json:"peak_mb"`
	ObjectSHA string  `json:"object_sha"`
	AsmSHA    string  `json:"asm_sha"`
}

func main() {
	memoPath := flag.String("memo", "", "memo file to read and write (empty: no persistence)")
	noMemo := flag.Bool("no-memo", false, "control arm: identical code path, store ignored")
	closure := flag.String("closure", "read", "which hits to demote to the live set: read | splice | none")
	object := flag.String("object", "", "write the object here")
	asm := flag.String("asm", "", "write the translated assembly here")
	jsonOut := flag.String("json", "", "append a JSON timing record here")
	digests := flag.String("digests", "", "write every function's finished body digest here")
	trace := flag.String("trace", "", "comma-separated function names to digest after every pass of the stage")
	verbose := flag.Bool("v", true, "print the phase table")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: memoc [-memo FILE] [-no-memo] [-object OUT.o] program.go")
		os.Exit(2)
	}
	path := flag.Arg(0)
	src, err := os.ReadFile(path)
	check(err)

	t := timings{Program: filepath.Base(path), Arm: "memo/" + *closure}
	if *noMemo {
		t.Arm = "control"
	}
	wall := time.Now()

	start := time.Now()
	module, err := goc.CompileExecutableFor(goc.TargetARM64, filepath.Base(path), src)
	check(err)
	t.FrontEnd = time.Since(start).Seconds()

	// One pipeline, split with one session. Rebuilding it across the split gives
	// jump threading a second per-function budget and moves three functions --
	// cmd/splitprobe measures exactly that.
	pipeline := opt.DefaultPipeline()
	session := opt.NewSession()

	start = time.Now()
	session.Run(module, pipeline[:opt.PerFunctionPrefixLen])
	t.Prefix = time.Since(start).Seconds()

	start = time.Now()
	store := memo.NewStore()
	if *memoPath != "" && !*noMemo {
		store, err = memo.Load(*memoPath)
		check(err)
	}
	t.StoreIO = time.Since(start).Seconds()

	var traceNames []string
	var traceTo func(string)
	var traceBody func(string, *ir.Func)
	if *trace != "" {
		traceNames = strings.Split(*trace, ",")
		traceTo = func(line string) { fmt.Fprintln(os.Stderr, "trace", line) }
		if dir := os.Getenv("MEMO_TRACE_DIR"); dir != "" {
			check(os.MkdirAll(dir, 0o755))
			traceBody = func(after string, f *ir.Func) {
				name := strings.ReplaceAll(f.Name, "/", "_")
				check(os.WriteFile(filepath.Join(dir, after+"."+name+".ir"), []byte(f.String()), 0o644))
			}
		}
	}
	var finalStates map[string]memo.FinalState
	if *digests != "" {
		finalStates = map[string]memo.FinalState{}
	}
	result, err := memo.RunStage(module, session, pipeline[opt.PerFunctionPrefixLen:], store, memo.Options{
		Pipeline:  opt.PipelineIdentity(),
		Target:    string(goc.TargetARM64),
		Compiler:  compilerIdentity(),
		Disabled:  *noMemo,
		Closure:   *closure,
		Digests:   finalStates,
		Trace:     traceNames,
		TraceTo:   traceTo,
		TraceBody: traceBody,
	})
	check(err)
	t.Digest = result.DigestTime.Seconds()
	t.Lookup = result.LookupTime.Seconds()
	t.Stage = result.StageTime.Seconds()
	t.Record = result.RecordTime.Seconds()
	t.Funcs, t.Hits, t.Misses = result.Funcs, result.Hits, result.Misses
	t.Excluded, t.DraggedIn, t.Survivors = result.Excluded, result.DraggedIn, result.Survivors
	t.StoreMB = float64(result.StoreBytes) / (1 << 20)
	t.Static = result.Static
	t.Whole = result.WholeModule

	// The back end is a pure function of the finished body, so its memo needs no
	// dependency set: the finished body's digest is the whole key.
	arm64.SetFunctionCodeCache(backendCache(result))

	start = time.Now()
	var objectBuf bytes.Buffer
	assembly, err := arm64.WriteObjectAndAssembly(&objectBuf, module)
	check(err)
	t.Backend = time.Since(start).Seconds()
	t.BackHits = arm64.FunctionCodeCacheHits()
	arm64.SetFunctionCodeCache(nil)

	t.ObjectSHA = fmt.Sprintf("%x", sha256.Sum256(objectBuf.Bytes()))[:16]
	t.AsmSHA = fmt.Sprintf("%x", sha256.Sum256([]byte(assembly)))[:16]
	if *object != "" {
		check(os.WriteFile(*object, objectBuf.Bytes(), 0o644))
	}
	if *asm != "" {
		check(os.WriteFile(*asm, []byte(assembly), 0o644))
	}

	start = time.Now()
	if *memoPath != "" && !*noMemo {
		check(store.Save(*memoPath))
		info, err := os.Stat(*memoPath)
		check(err)
		t.MemoMB = float64(info.Size()) / (1 << 20)
	}
	t.StoreIO += time.Since(start).Seconds()
	t.Total = time.Since(wall).Seconds()

	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	t.PeakMB = float64(stats.Sys) / (1 << 20)

	if *verbose {
		fmt.Fprintf(os.Stderr, "%s %s: %.2fs total\n", t.Program, t.Arm, t.Total)
		fmt.Fprintf(os.Stderr, "  front end %.2f  prefix %.2f  digest %.2f  lookup %.2f  stage %.2f  record %.2f  backend %.2f  store-io %.2f\n",
			t.FrontEnd, t.Prefix, t.Digest, t.Lookup, t.Stage, t.Record, t.Backend, t.StoreIO)
		if result.WholeModule {
			fmt.Fprintln(os.Stderr, "  whole-module hit: every function matched, the stage was skipped outright")
		}
		fmt.Fprintf(os.Stderr, "  %d entries the stage does not move (input digest == output digest)\n", t.Static)
		fmt.Fprintf(os.Stderr, "  %d funcs: %d hits, %d misses, %d nosplit-excluded, %d dragged in; %d survive; backend %d/%d cached\n",
			t.Funcs, t.Hits, t.Misses, t.Excluded, t.DraggedIn, t.Survivors, t.BackHits, t.Survivors)
		if len(result.MissReasons) > 0 {
			reasons := make([]string, 0, len(result.MissReasons))
			for r := range result.MissReasons {
				reasons = append(reasons, r)
			}
			sort.Strings(reasons)
			for _, r := range reasons {
				fmt.Fprintf(os.Stderr, "    miss: %-32s %d\n", r, result.MissReasons[r])
			}
		}
		fmt.Fprintf(os.Stderr, "  object %s  asm %s  memo %.1f MB on disk\n", t.ObjectSHA, t.AsmSHA, t.MemoMB)
	}
	if *jsonOut != "" {
		appendJSON(*jsonOut, t)
	}
	if *digests != "" {
		names := make([]string, 0, len(finalStates))
		for name := range finalStates {
			names = append(names, name)
		}
		sort.Strings(names)
		var buf bytes.Buffer
		for _, name := range names {
			state := finalStates[name]
			live := "live"
			if !state.Survives {
				live = "dropped"
			}
			fmt.Fprintf(&buf, "%s %s %s\n", state.Digest, live, name)
		}
		check(os.WriteFile(*digests, buf.Bytes(), 0o644))
	}
	if len(result.MissedNames) > 0 && len(result.MissedNames) <= 64 {
		sort.Strings(result.MissedNames)
		fmt.Fprintf(os.Stderr, "  invalidated: %v\n", result.MissedNames)
	}
}

// backendCache keys the finished per-function code on the finished body's
// digest.
func backendCache(result *memo.Result) func(*ir.Func) ([32]byte, bool) {
	return func(f *ir.Func) ([32]byte, bool) {
		d, ok := result.OutputOf[f]
		return d, ok
	}
}

// compilerIdentity hashes the running binary, which is what the key means when
// it says "the same compiler". gc's buildActionID does the same thing.
func compilerIdentity() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))[:32]
}

func appendJSON(path string, t timings) {
	line, err := json.Marshal(t)
	check(err)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	check(err)
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	check(err)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "memoc:", err)
		os.Exit(1)
	}
}
