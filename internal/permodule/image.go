package permodule

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/gometa"
	"github.com/evanphx/cg12/link"
	"github.com/evanphx/cg12/obj"
)

// Mode selects the shape of the linked image. The two non-default modes are
// controls: each reproduces what the image looked like before one half of the
// per-module mechanism existed, and each must fail.
type Mode string

const (
	// ModePerModule chains a second moduledata, so each module resolves its own
	// type offsets against its own base. This is the shipped scheme.
	ModePerModule Mode = "permodule"

	// ModeFlat leaves the chain at length one and stretches the program module's
	// etypes over both objects, which is the single flat type region cg12 had
	// before. The second module's baked offsets are then read against the wrong
	// base.
	ModeFlat Mode = "flat"

	// ModeSharedTextEnd chains the module but bounds its text with the *first*
	// module's text-end symbol, which is what a single global text-end symbol
	// produced.
	ModeSharedTextEnd Mode = "sharedtext"
)

// ImageOptions describes the two-module image to build.
type ImageOptions struct {
	// SourcePath is the Go program goc compiles as the first module.
	SourcePath string

	// Mode selects the scheme or one of its controls.
	Mode Mode

	// Pad inserts that many bytes of foreign data between the two modules, so a
	// run can show the result does not depend on where the second module lands.
	Pad int

	// WithoutTypeLinks builds the second module with no moduledata.typelinks.
	WithoutTypeLinks bool

	// Report, when non-nil, receives one line per interesting layout fact.
	Report func(string)
}

// BuildImage compiles the program with goc, builds the second module, links them
// with cg12's own linker and returns the executable image.
//
// No system linker is involved: the only external tool is `cc -c`, to assemble
// the Plan 9 sidecar the Go runtime needs and the process entry stub.
func BuildImage(options ImageOptions) ([]byte, error) {
	if options.Mode == "" {
		options.Mode = ModePerModule
	}
	switch options.Mode {
	case ModePerModule, ModeFlat, ModeSharedTextEnd:
	default:
		return nil, fmt.Errorf("unknown mode %q", options.Mode)
	}
	report := options.Report
	if report == nil {
		report = func(string) {}
	}

	source, err := os.ReadFile(options.SourcePath)
	if err != nil {
		return nil, err
	}
	program, err := goc.CompileExecutableFor(goc.TargetARM64, filepath.Base(options.SourcePath), source)
	if err != nil {
		return nil, fmt.Errorf("goc: %w", err)
	}
	programObject, assembly, err := arm64.CompileObjectAndAssembly(program)
	if err != nil {
		return nil, fmt.Errorf("arm64: %w", err)
	}

	work, err := os.MkdirTemp("", "permodule")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	sidecar, err := assembleSource(work, "goc-runtime", assembly)
	if err != nil {
		return nil, err
	}
	stubs, err := assembleSource(work, "goc-stubs", runtimeStubs)
	if err != nil {
		return nil, err
	}

	second, err := arm64.CompileToObject(Build(Options{WithoutTypeLinks: options.WithoutTypeLinks}))
	if err != nil {
		return nil, fmt.Errorf("second module: %w", err)
	}
	reportSecondModule(report, second)

	linker := link.New()
	if err := linker.AddObjectFile(programObject); err != nil {
		return nil, fmt.Errorf("add program: %w", err)
	}
	if err := linker.AddObjectFile(sidecar); err != nil {
		return nil, fmt.Errorf("add sidecar: %w", err)
	}
	if err := linker.AddObjectFile(stubs); err != nil {
		return nil, fmt.Errorf("add stubs: %w", err)
	}
	secondIndex := 3
	if options.Pad > 0 {
		linker.AddObject(&obj.Object{Machine: obj.EM_AARCH64, Data: make([]byte, options.Pad), DataAlign: 8})
		secondIndex = 4
	}
	linker.AddObject(second)

	merged, err := linker.Link()
	if err != nil {
		return nil, fmt.Errorf("link: %w", err)
	}
	if err := reportShift(report, merged, secondIndex); err != nil {
		return nil, err
	}
	if err := wire(merged, options.Mode, secondIndex, report); err != nil {
		return nil, err
	}
	image, err := merged.WriteExecutable("_gocstart")
	if err != nil {
		return nil, fmt.Errorf("write executable: %w", err)
	}
	return image, nil
}

// moduledata field offsets the link step edits. Verified with unsafe.Offsetof
// against the host toolchain rather than counted by eye.
const (
	moduledataMaxPC   = 168
	moduledataEtext   = 184
	moduledataEtypes  = 304
	moduledataHasMain = 536
)

// wire performs the link-step edits, all of them ordinary R_AARCH64_ABS64 data
// relocations against the merged object.
//
//   - Each program slot naming a foreign symbol is pointed at it, and the second
//     module's callback word is pointed back at the program's function. Every one
//     of these is an 8-byte pointer, which was never the problem: pointers cross
//     modules freely, and only NameOff/TypeOff must stay within one.
//   - The second module is joined to the image by one write to
//     runtime.firstmoduledata.next. That is the whole of the join.
func wire(merged *obj.Object, mode Mode, secondIndex int, report func(string)) error {
	for _, slot := range []struct{ program, foreign string }{
		{"main_probeSlot", WidgetType},
		{"main_probeIntSlot", IntType},
		{"main_probeFuncSlot", EntryFuncValue},
		{"main_probeCodeSlot", EntryFunc},
		{"main_probeHoldSlot", HoldFuncValue},
	} {
		if _, err := findSymbol(merged, slot.program, 0); err != nil {
			continue
		}
		if err := patch(merged, slot.program, 0, 0, linkerName(slot.foreign), secondIndex); err != nil {
			return err
		}
	}
	// The second module calls back into the program through a patched word,
	// because the program's own functions are local symbols the linker namespaces
	// per object.
	if _, err := findSymbol(merged, "main_probeCallback", 0); err == nil {
		if err := patch(merged, linkerName(CallbackSlot), secondIndex, 0, "main_probeCallback", 0); err != nil {
			return err
		}
	}

	firstModule, err := findSymbol(merged, "runtime_firstmoduledata", 0)
	if err != nil {
		return err
	}
	secondModule, err := findSymbol(merged, linkerName(DataSymbol), secondIndex)
	if err != nil {
		return err
	}
	report(fmt.Sprintf("hasmain: program module %d, second module %d",
		merged.Data[firstModule.Value+moduledataHasMain],
		merged.Data[secondModule.Value+moduledataHasMain]))

	switch mode {
	case ModePerModule:
		return gometa.ChainModule(merged, firstModule.Name, secondModule.Name, obj.R_AARCH64_ABS64)
	case ModeSharedTextEnd:
		if err := gometa.ChainModule(merged, firstModule.Name, secondModule.Name, obj.R_AARCH64_ABS64); err != nil {
			return err
		}
		// Point the second module's maxpc and etext at the first module's text
		// end, which is what one global text-end symbol produced. Its own text then
		// lies outside [minpc, maxpc), so runtime.findmoduledatap never selects it.
		return retarget(merged, gometa.DefaultTextEndSymbol,
			secondModule.Value+moduledataMaxPC, secondModule.Value+moduledataEtext)
	}
	// flat: one module, one type region, stretched over both objects.
	secondEnd, err := findSymbol(merged, linkerName(DataEnd), secondIndex)
	if err != nil {
		return err
	}
	return retarget(merged, secondEnd.Name, firstModule.Value+moduledataEtypes)
}

func patch(merged *obj.Object, holder string, holderIndex int, field uint64, target string, targetIndex int) error {
	symbol, err := findSymbol(merged, holder, holderIndex)
	if err != nil {
		return err
	}
	if symbol.Section != obj.SecData {
		return fmt.Errorf("%s is in section %d, not .data", holder, symbol.Section)
	}
	resolved, err := findSymbol(merged, target, targetIndex)
	if err != nil {
		return err
	}
	merged.DataRelocs = append(merged.DataRelocs, obj.Reloc{
		Offset: symbol.Value + field, Sym: resolved.Name, Type: obj.R_AARCH64_ABS64,
	})
	return nil
}

// retarget rewrites the existing relocations at the given data offsets to name a
// different symbol.
func retarget(merged *obj.Object, symbol string, offsets ...uint64) error {
	wanted := make(map[uint64]bool, len(offsets))
	for _, offset := range offsets {
		wanted[offset] = true
	}
	found := 0
	for index := range merged.DataRelocs {
		if wanted[merged.DataRelocs[index].Offset] {
			merged.DataRelocs[index].Sym = symbol
			found++
		}
	}
	if found != len(offsets) {
		return fmt.Errorf("retargeted %d of %d moduledata relocations", found, len(offsets))
	}
	return nil
}

func reportSecondModule(report func(string), second *obj.Object) {
	base, ok := symbolValue(second, DataStart)
	if !ok {
		report("second module has no data-start symbol")
		return
	}
	end, _ := symbolValue(second, DataEnd)
	widget, _ := symbolValue(second, WidgetType)
	report(fmt.Sprintf("second module: %d bytes of .text, %d bytes of .data, datastart at %d, Widget at +%d",
		len(second.Text), len(second.Data), base, widget-base))
	report(fmt.Sprintf("second module type region spans [+0, +%d)", end-base))
}

// reportShift prints how far the second module's data moved between the object it
// was compiled into (where its base is offset 0) and the merged image. That
// distance is the amount by which every one of its baked offsets would be wrong
// if they were read against the program's type base instead of its own.
func reportShift(report func(string), merged *obj.Object, secondIndex int) error {
	programBase, err := findSymbol(merged, linkerName(".goc.runtime.datastart"), 0)
	if err != nil {
		return err
	}
	secondBase, err := findSymbol(merged, linkerName(DataStart), secondIndex)
	if err != nil {
		return err
	}
	report(fmt.Sprintf("merged .data is %d bytes; program base at %d, second module's base at %d (shifted %d bytes from where it was compiled)",
		len(merged.Data), programBase.Value, secondBase.Value, secondBase.Value-programBase.Value))
	return nil
}

// findSymbol resolves a name the linker may have namespaced. link.merge renames
// every local symbol to "<name>.<object index>" so one object's statics cannot
// capture another's references, so a local is found under a suffix.
func findSymbol(merged *obj.Object, name string, objectIndex int) (obj.Sym, error) {
	namespaced := fmt.Sprintf("%s.%d", name, objectIndex)
	for _, symbol := range merged.Syms {
		if symbol.Name == name || symbol.Name == namespaced {
			return symbol, nil
		}
	}
	return obj.Sym{}, fmt.Errorf("symbol %q (or %q) is not defined in the merged object", name, namespaced)
}

func symbolValue(object *obj.Object, name string) (uint64, bool) {
	target := linkerName(name)
	for _, symbol := range object.Syms {
		if symbol.Name == target {
			return symbol.Value, true
		}
	}
	return 0, false
}

func linkerName(name string) string {
	var builder strings.Builder
	for _, r := range name {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	return builder.String()
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
