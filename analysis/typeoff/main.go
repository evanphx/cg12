// Command typeoff builds a two-module goc image -- the program compiled by goc
// as usual, plus a separately compiled object carrying a Go module of its own --
// and runs the result.
//
// It began as the design spike's prototype (ccwork/typeoff-alternatives) and is
// now a driver for the landed mechanism: the second module is built by
// internal/permodule and needs no hand-added symbols. The link step's only
// structural edit is one word, runtime.firstmoduledata.next.
//
//	-mode=permodule   chain the second moduledata (the shipped scheme)
//	-mode=flat        leave one module spanning both objects (the pre-change
//	                  scheme), which is the control: it must fail
//	-mode=sharedtext  chain the module but give it the first module's text end,
//	                  which is what a global text-end symbol produced
//	-notypelinks      build the second module without moduledata.typelinks
//	-pad=N            insert N bytes of foreign data before the second module,
//	                  to show the result does not depend on where it lands
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/evanphx/cg12/internal/permodule"
)

func main() {
	mode := flag.String("mode", "permodule", "permodule | flat | sharedtext")
	pad := flag.Int("pad", 0, "bytes of foreign data to insert before the second module")
	noTypeLinks := flag.Bool("notypelinks", false, "build the second module without moduledata.typelinks")
	out := flag.String("o", "", "output executable")
	run := flag.Bool("run", true, "run the resulting executable")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: typeoff [-mode permodule|flat|sharedtext] [-pad N] [-notypelinks] [-o exe] file.go")
		os.Exit(2)
	}
	if err := buildAndRun(flag.Arg(0), permodule.Mode(*mode), *pad, *noTypeLinks, *out, *run); err != nil {
		fmt.Fprintf(os.Stderr, "typeoff: %v\n", err)
		os.Exit(1)
	}
}

func buildAndRun(input string, mode permodule.Mode, pad int, noTypeLinks bool, out string, run bool) error {
	image, err := permodule.BuildImage(permodule.ImageOptions{
		SourcePath:       input,
		Mode:             mode,
		Pad:              pad,
		WithoutTypeLinks: noTypeLinks,
		Report:           func(line string) { fmt.Println("typeoff:", line) },
	})
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "typeoff")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	if out == "" {
		out = filepath.Join(work, "a.out")
	}
	if err := os.WriteFile(out, image, 0o755); err != nil {
		return err
	}
	fmt.Printf("typeoff: mode=%s pad=%d typelinks=%v image=%d bytes -> %s\n",
		mode, pad, !noTypeLinks, len(image), out)
	if !run {
		return nil
	}
	command := exec.Command(out)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	err = command.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		fmt.Printf("typeoff: exit status %d\n", exit.ExitCode())
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Println("typeoff: exit status 0")
	return nil
}
