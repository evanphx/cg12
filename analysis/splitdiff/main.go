package main

// splitdiff compiles every named program both ways -- monolithic and against a
// prebuilt runtime -- runs both, and reports any difference in exit status,
// stdout or stderr. It is throwaway measurement, not a shipped tool.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/prebuilt"
	"github.com/evanphx/cg12/internal/runtimepack"
)

type result struct {
	name                      string
	splitCompile, monoCompile time.Duration
	splitLink, monoLink       time.Duration
	splitSize, monoSize       int64
	status                    string
}

func main() {
	work := flag.String("work", "", "scratch directory")
	workers := flag.Int("j", 8, "workers")
	runIt := flag.Bool("run", true, "run both binaries and compare")
	flag.Parse()
	programs := flag.Args()

	start := time.Now()
	pack, err := prebuilt.BuildRuntime(goc.TargetARM64, prebuilt.Options{})
	if err != nil {
		panic(err)
	}
	fmt.Printf("build-runtime: %.2fs object=%d sidecar=%d defined=%d\n",
		time.Since(start).Seconds(), len(pack.Object), len(pack.Sidecar), len(pack.Manifest.Defined))
	runtimeObject := filepath.Join(*work, "rt.o")
	sidecarObject := filepath.Join(*work, "rtasm.o")
	os.WriteFile(runtimeObject, pack.Object, 0o644)
	os.WriteFile(sidecarObject, pack.Sidecar, 0o644)

	results := make([]result, len(programs))
	var wg sync.WaitGroup
	slots := make(chan struct{}, *workers)
	for index, path := range programs {
		wg.Add(1)
		go func(index int, path string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[index] = one(*work, path, pack, runtimeObject, sidecarObject, *runIt)
		}(index, path)
	}
	wg.Wait()

	var splitTotal, monoTotal time.Duration
	var splitBytes, monoBytes int64
	bad := 0
	for _, r := range results {
		splitTotal += r.splitCompile + r.splitLink
		monoTotal += r.monoCompile + r.monoLink
		splitBytes += r.splitSize
		monoBytes += r.monoSize
		if r.status != "OK" {
			bad++
			fmt.Printf("%-44s %s\n", r.name, r.status)
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].monoCompile > results[j].monoCompile })
	fmt.Println("--- slowest monolithic compiles ---")
	for i, r := range results {
		if i >= 10 {
			break
		}
		fmt.Printf("%-44s split=%6.2fs mono=%6.2fs\n", r.name, (r.splitCompile + r.splitLink).Seconds(), (r.monoCompile + r.monoLink).Seconds())
	}
	fmt.Printf("programs=%d  problems=%d\n", len(results), bad)
	fmt.Printf("total CPU compile+link: split=%.1fs mono=%.1fs  ratio=%.2fx\n",
		splitTotal.Seconds(), monoTotal.Seconds(), monoTotal.Seconds()/splitTotal.Seconds())
	fmt.Printf("total image bytes: split=%d mono=%d  (%.1f%%)\n",
		splitBytes, monoBytes, 100*float64(splitBytes-monoBytes)/float64(monoBytes))
}

func one(work, path string, pack *runtimepack.Pack, runtimeObject, sidecarObject string, runIt bool) result {
	name := filepath.Base(path)
	r := result{name: name, status: "OK"}
	src, err := os.ReadFile(path)
	if err != nil {
		r.status = "READ FAILED: " + err.Error()
		return r
	}
	t0 := time.Now()
	object, err := prebuilt.CompileProgram(goc.TargetARM64, name, src, []*runtimepack.Manifest{&pack.Manifest}, prebuilt.Options{})
	r.splitCompile = time.Since(t0)
	if err != nil {
		r.status = "SPLIT COMPILE: " + err.Error()
		return r
	}
	programObject := filepath.Join(work, name+".o")
	os.WriteFile(programObject, object.Object, 0o644)
	linkInputs := []string{"-no-pie", "-o", filepath.Join(work, name+".split"), runtimeObject, sidecarObject, programObject}
	if len(object.Sidecar) > 0 {
		programSidecar := filepath.Join(work, name+".sidecar.o")
		os.WriteFile(programSidecar, object.Sidecar, 0o644)
		linkInputs = append(linkInputs, programSidecar)
	}
	splitExe := filepath.Join(work, name+".split")
	t1 := time.Now()
	out, err := exec.Command("cc", linkInputs...).CombinedOutput()
	r.splitLink = time.Since(t1)
	if err != nil {
		r.status = "SPLIT LINK: " + err.Error() + " " + trim(string(out))
		return r
	}

	t2 := time.Now()
	module, err := goc.CompileExecutableFor(goc.TargetARM64, name, src)
	if err != nil {
		r.status = "MONO COMPILE: " + err.Error()
		return r
	}
	monoObject := filepath.Join(work, name+".mono.o")
	file, _ := os.Create(monoObject)
	assembly, err := arm64.WriteObjectAndAssembly(file, module)
	file.Close()
	r.monoCompile = time.Since(t2)
	if err != nil {
		r.status = "MONO EMIT: " + err.Error()
		return r
	}
	t3 := time.Now()
	asmSource := filepath.Join(work, name+".mono.S")
	asmObject := filepath.Join(work, name+".mono.asm.o")
	os.WriteFile(asmSource, []byte(assembly), 0o644)
	if out, err := exec.Command("cc", "-c", "-o", asmObject, asmSource).CombinedOutput(); err != nil {
		r.status = "MONO ASM: " + err.Error() + " " + trim(string(out))
		return r
	}
	monoExe := filepath.Join(work, name+".mono")
	if out, err := exec.Command("cc", "-no-pie", "-o", monoExe, monoObject, asmObject).CombinedOutput(); err != nil {
		r.status = "MONO LINK: " + err.Error() + " " + trim(string(out))
		return r
	}
	r.monoLink = time.Since(t3)
	if info, err := os.Stat(splitExe); err == nil {
		r.splitSize = info.Size()
	}
	if info, err := os.Stat(monoExe); err == nil {
		r.monoSize = info.Size()
	}
	if !runIt {
		return r
	}
	splitOutput, splitCode := run(splitExe)
	monoOutput, monoCode := run(monoExe)
	if splitCode != monoCode || splitOutput != monoOutput {
		r.status = fmt.Sprintf("DIFFERS: mono rc=%d split rc=%d\n  mono : %s\n  split: %s",
			monoCode, splitCode, trim(monoOutput), trim(splitOutput))
	}
	return r
}

// run executes one build of a program under a deadline.
//
// The timeout is not optional. RUNTIME_PLAN 5.10 records rare unexplained hangs on
// both the base and the fixed compiler, so a harness that runs the corpus without
// one stalls rather than reporting -- and a stall is indistinguishable from slow
// progress on the programs that legitimately take minutes.
func run(path string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, path)
	command.Env = append(os.Environ(), "GOMAXPROCS=2")
	output, err := command.CombinedOutput()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		code = -1
	}
	if ctx.Err() != nil {
		return string(output) + "\n[timed out]", -2
	}
	return string(output), code
}

const runTimeout = 120 * time.Second

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}
