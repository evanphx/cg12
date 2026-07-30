package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/prebuilt"
	"github.com/evanphx/cg12/internal/runtimepack"
)

// buildRuntimeCommand implements `goc build-runtime -o runtime.gocrt`: it
// compiles the Go runtime once, as a Go module of its own, so that every program
// linked against it can skip compiling the runtime again.
//
// The result is not a bare object. A program compiled against it has to know
// exactly which symbols it defines, so the objects and that manifest travel
// together in one versioned file -- see internal/runtimepack for why that beats
// an archive or a bare ELF.
func buildRuntimeCommand(arguments []string, errorOutput io.Writer) int {
	flags := flag.NewFlagSet("goc build-runtime", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	output := flags.String("o", "", "write the prebuilt runtime here")
	optimize := flags.Bool("O", false, "optimize cg12 IR")
	targetName := flags.String("target", defaultTargetName(), "arm64")
	packages := flags.String("packages", strings.Join(prebuilt.DefaultPackages, ","),
		"comma-separated standard library packages the pack carries beyond the runtime; \"\" for the runtime alone")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *output == "" {
		fmt.Fprintln(errorOutput, "usage: goc build-runtime [-O] [-target arch] [-packages list] -o runtime.gocrt")
		return 2
	}
	target, err := goc.ParseTarget(*targetName)
	if err != nil {
		fmt.Fprintf(errorOutput, "%v\n", err)
		return 1
	}
	pack, err := prebuilt.BuildRuntime(target, prebuilt.Options{
		Optimize: *optimize,
		Packages: splitPackageList(*packages),
	})
	if err != nil {
		fmt.Fprintf(errorOutput, "goc: %v\n", err)
		return 1
	}
	if err := pack.Write(*output); err != nil {
		fmt.Fprintf(errorOutput, "goc: %v\n", err)
		return 1
	}
	return 0
}

// splitPackageList parses the -packages flag. An empty or whitespace-only entry
// is dropped rather than passed through, so `-packages ""` and `-packages " "`
// both mean "the runtime alone".
func splitPackageList(value string) []string {
	var packages []string
	for _, field := range strings.Split(value, ",") {
		path := strings.TrimSpace(field)
		if path == "" {
			continue
		}
		packages = append(packages, path)
	}
	return packages
}

// linkAgainstPrebuiltRuntime compiles one program as a second Go module and links
// it with the prebuilt runtime.
//
// The object order is load-bearing. Each module's moduledata records a [minpc,
// maxpc) that runtime.findmoduledatap resolves a PC against, so a module's text
// has to be contiguous: the prebuilt object and the sidecar that ends its text
// come first and adjacent, and the program's text follows.
func linkAgainstPrebuiltRuntime(target goc.Target, packPath, name string, source []byte, executable string, optimize bool) error {
	pack, err := runtimepack.Read(packPath)
	if err != nil {
		return err
	}
	if pack.Manifest.Optimize != optimize {
		return fmt.Errorf("the prebuilt runtime was built with -O=%v, but this program is being compiled with -O=%v",
			pack.Manifest.Optimize, optimize)
	}
	program, err := prebuilt.CompileProgram(target, filepath.Base(name), source, pack, prebuilt.Options{Optimize: optimize})
	if err != nil {
		return err
	}
	// A temporary directory with fixed names inside, rather than temporary files
	// with random ones: cc records an object's filename in the linked binary, so
	// random names make two builds of the same source differ.
	work, err := os.MkdirTemp("", "cg12-goc-split")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	runtimeObject := filepath.Join(work, "goc-runtime-module.o")
	sidecarObject := filepath.Join(work, "goc-runtime.o")
	programObject := filepath.Join(work, "goc.o")
	inputs := map[string][]byte{
		runtimeObject: pack.Object,
		sidecarObject: pack.Sidecar,
		programObject: program.Object,
	}
	linkInputs := []string{runtimeObject, sidecarObject, programObject}
	if len(program.Sidecar) > 0 {
		programSidecar := filepath.Join(work, "goc-program-runtime.o")
		inputs[programSidecar] = program.Sidecar
		linkInputs = append(linkInputs, programSidecar)
	}
	for path, contents := range inputs {
		if err := os.WriteFile(path, contents, 0o644); err != nil {
			return err
		}
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		return err
	}
	command := exec.Command(cc, append([]string{"-no-pie", "-o", executable}, linkInputs...)...)
	command.Stderr = os.Stderr
	return command.Run()
}
