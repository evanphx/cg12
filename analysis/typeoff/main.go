// Command typeoff is the working prototype for the type-descriptor-offset
// design spike. It builds a two-module goc image: the program compiled by goc
// as usual, plus a *separately compiled* object that carries Go type
// descriptors of its own, and it drives the program through reflect against a
// type that lives in the second module.
//
// The question the spike asks is how a type descriptor's 32-bit NameOff and
// TypeOff fields can stay correct once more than one object contributes type
// descriptors. cg12 resolves them in the back end
// (arm64.resolveRelativeDataFixups) as `value(target) - value(datastart)` and
// leaves no relocation, so the number is right for exactly one layout.
//
// The scheme under test -- per-module type regions, which is Go's own answer --
// changes nothing about that arithmetic. It gives the second object its own
// `moduledata` whose `types`/`etypes` bound its own data, and chains it onto
// runtime.firstmoduledata. runtime.resolveNameOff and runtime.resolveTypeOff
// already pick the module that contains the *referring* type and add the offset
// to that module's base, so a module-local offset is correct wherever the
// module lands.
//
//	-mode=permodule   chain the second moduledata (the proposed scheme)
//	-mode=flat        leave one module spanning both objects (today's scheme),
//	                  which is the control: it must fail
//	-pad=N            insert N bytes of foreign data before the second module,
//	                  to show the result does not depend on where it lands
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/link"
	"github.com/evanphx/cg12/obj"
)

func main() {
	mode := flag.String("mode", "permodule", "permodule | flat")
	pad := flag.Int("pad", 0, "bytes of foreign data to insert before the second module")
	out := flag.String("o", "", "output executable")
	run := flag.Bool("run", true, "run the resulting executable")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: typeoff [-mode permodule|flat] [-pad N] [-o exe] file.go")
		os.Exit(2)
	}
	if err := buildAndRun(flag.Arg(0), *mode, *pad, *out, *run); err != nil {
		fmt.Fprintf(os.Stderr, "typeoff: %v\n", err)
		os.Exit(1)
	}
}

func buildAndRun(input, mode string, pad int, out string, run bool) error {
	if mode != "permodule" && mode != "flat" {
		return fmt.Errorf("unknown mode %q", mode)
	}
	source, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	module, err := goc.CompileExecutableFor(goc.TargetARM64, filepath.Base(input), source)
	if err != nil {
		return fmt.Errorf("goc: %w", err)
	}
	programObject, assembly, err := arm64.CompileObjectAndAssembly(module)
	if err != nil {
		return fmt.Errorf("arm64: %w", err)
	}

	work, err := os.MkdirTemp("", "typeoff")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	sidecar, err := assembleSource(work, "goc-runtime", assembly)
	if err != nil {
		return err
	}
	stubs, err := assembleSource(work, "goc-stubs", runtimeStubs)
	if err != nil {
		return err
	}

	probe, err := arm64.CompileToObject(buildProbeModule())
	if err != nil {
		return fmt.Errorf("probe module: %w", err)
	}
	reportProbeOffsets(probe)

	linker := link.New()
	if err := linker.AddObjectFile(programObject); err != nil {
		return fmt.Errorf("add program: %w", err)
	}
	if err := linker.AddObjectFile(sidecar); err != nil {
		return fmt.Errorf("add sidecar: %w", err)
	}
	if err := linker.AddObjectFile(stubs); err != nil {
		return fmt.Errorf("add stubs: %w", err)
	}
	if pad > 0 {
		linker.AddObject(&obj.Object{
			Machine: obj.EM_AARCH64, Data: make([]byte, pad), DataAlign: 8,
		})
	}
	linker.AddObject(probe)

	merged, err := linker.Link()
	if err != nil {
		return fmt.Errorf("link: %w", err)
	}
	if err := reportShift(merged); err != nil {
		return err
	}
	if err := wire(merged, mode); err != nil {
		return err
	}
	image, err := merged.WriteExecutable("_gocstart")
	if err != nil {
		return fmt.Errorf("write executable: %w", err)
	}
	if out == "" {
		out = filepath.Join(work, "a.out")
	}
	if err := os.WriteFile(out, image, 0o755); err != nil {
		return err
	}
	fmt.Printf("typeoff: mode=%s pad=%d image=%d bytes -> %s\n", mode, pad, len(image), out)
	if !run {
		return nil
	}
	return runImage(out)
}

// wire performs the two link-step edits the prototype needs, both expressed as
// ordinary R_AARCH64_ABS64 data relocations against the merged object.
//
//   - `probeSlot`, the program's package-level word, is pointed at the second
//     module's type descriptor. This stands in for whatever would really carry a
//     cross-module type reference; the point is that it is an 8-byte pointer,
//     which has never been the problem.
//   - in permodule mode, runtime.firstmoduledata.next is pointed at the second
//     module's moduledata, which is all it takes for runtime.modulesinit to pick
//     it up. In flat mode that link is left nil and the program's own `etypes` is
//     stretched to cover the second module instead, which is exactly what today's
//     single flat type region does -- and is the control that must fail.
func wire(merged *obj.Object, mode string) error {
	slot, err := findSymbol(merged, "main_probeSlot")
	if err != nil {
		return err
	}
	if slot.Section != obj.SecData {
		return fmt.Errorf("probeSlot is in section %d, not .data", slot.Section)
	}
	merged.DataRelocs = append(merged.DataRelocs, obj.Reloc{
		Offset: slot.Value, Sym: linkerName(probeWidgetType), Type: obj.R_AARCH64_ABS64,
	})

	moduledata, err := findSymbol(merged, "runtime_firstmoduledata")
	if err != nil {
		return err
	}
	if mode == "permodule" {
		merged.DataRelocs = append(merged.DataRelocs, obj.Reloc{
			Offset: moduledata.Value + moduledataNextField,
			Sym:    linkerName(probeModuleData), Type: obj.R_AARCH64_ABS64,
		})
		return nil
	}
	// flat: one module, one type region, stretched over both objects.
	target := moduledata.Value + moduledataEtypes
	for index := range merged.DataRelocs {
		if merged.DataRelocs[index].Offset == target {
			merged.DataRelocs[index].Sym = linkerName(probeDataEnd)
			return nil
		}
	}
	return fmt.Errorf("no relocation found at moduledata.etypes (offset %d)", target)
}

// reportShift prints how far the second module's data moved between the object
// it was compiled into (where its base is offset 0) and the merged image. That
// distance is the amount by which every one of its baked offsets would be wrong
// if they were read against the program's type base instead of its own.
func reportShift(merged *obj.Object) error {
	programBase, err := findSymbol(merged, linkerName(".goc.runtime.datastart"))
	if err != nil {
		return err
	}
	probeBase, err := findSymbol(merged, linkerName(probeDataStart))
	if err != nil {
		return err
	}
	fmt.Printf("typeoff: merged .data is %d bytes; program base at %d, second module's base at %d (shifted %d bytes from where it was compiled)\n",
		len(merged.Data), programBase.Value, probeBase.Value, probeBase.Value-programBase.Value)
	return nil
}

// findSymbol resolves a name the linker may have namespaced. link.merge renames
// every local symbol to "<name>.<object index>" so one object's statics cannot
// capture another's references, so a local is found under a suffix.
func findSymbol(o *obj.Object, name string) (obj.Sym, error) {
	var matches []obj.Sym
	for _, symbol := range o.Syms {
		if symbol.Name == name || strings.HasPrefix(symbol.Name, name+".") {
			matches = append(matches, symbol)
		}
	}
	if len(matches) == 0 {
		return obj.Sym{}, fmt.Errorf("symbol %q is not defined in the merged object", name)
	}
	if len(matches) > 1 {
		return obj.Sym{}, fmt.Errorf("symbol %q is ambiguous (%d definitions)", name, len(matches))
	}
	return matches[0], nil
}

func linkerName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// reportProbeOffsets prints the offsets the back end baked into the second
// module, so the run can be read against them. These are the numbers that are
// wrong under a single flat type region and right under a per-module one.
func reportProbeOffsets(probe *obj.Object) {
	base, ok := symbolValue(probe, probeDataStart)
	if !ok {
		fmt.Println("typeoff: probe module has no datastart")
		return
	}
	widget, _ := symbolValue(probe, probeWidgetType)
	end, _ := symbolValue(probe, probeDataEnd)
	fmt.Printf("typeoff: probe module: %d bytes of .data, datastart at %d, Widget at +%d\n",
		len(probe.Data), base, widget-base)
	fmt.Printf("typeoff: probe module type region spans [+0, +%d)\n", end-base)
}

func symbolValue(o *obj.Object, name string) (uint64, bool) {
	target := linkerName(name)
	for _, symbol := range o.Syms {
		if symbol.Name == target {
			return symbol.Value, true
		}
	}
	return 0, false
}

func assembleSource(work, name, source string) ([]byte, error) {
	sourcePath := filepath.Join(work, name+".S")
	objectPath := filepath.Join(work, name+".o")
	if err := os.WriteFile(sourcePath, []byte(source), 0o644); err != nil {
		return nil, err
	}
	assemble := exec.Command("cc", "-c", "-o", objectPath, sourcePath)
	assemble.Stderr = os.Stderr
	if err := assemble.Run(); err != nil {
		return nil, fmt.Errorf("assemble %s: %w", name, err)
	}
	return os.ReadFile(objectPath)
}

func runImage(path string) error {
	command := exec.Command(path)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	err := command.Run()
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

// runtimeStubs supplies what a static link with no C runtime lacks: `abort`,
// which cg12 lowers a few unreachable-by-construction paths to, and a process
// entry point handing the Go runtime's main the argc/argv Linux leaves on the
// stack. Both are from analysis/seplink, where they were established to be the
// only two things a cc-free goc link needs.
const runtimeStubs = `
	.text
	.globl abort
	.type abort, @function
abort:
	mov x0, #134
	mov x8, #94
	svc #0
	b abort

	.globl _gocstart
	.type _gocstart, @function
_gocstart:
	ldr x0, [sp]
	add x1, sp, #8
	bl main
	mov x8, #94
	svc #0
`
