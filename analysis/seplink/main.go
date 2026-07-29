// Command seplink probes what cg12's own linker can do with goc output, for the
// separate-compilation feasibility spike. It is throwaway instrumentation and
// changes no compiler behaviour.
//
// Two experiments:
//
//	-mode=native   compile one program, assemble its Plan 9 sidecar with cc -c,
//	               and link the pair with link.LinkExecutable instead of cc.
//	-mode=split    compile one program, cut the resulting object in two along a
//	               symbol boundary, and link the halves back together — which is
//	               exactly the cross-object resolution separate compilation needs.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/link"
	"github.com/evanphx/cg12/obj"
)

func main() {
	mode := flag.String("mode", "native", "native | split")
	out := flag.String("o", "", "output executable")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: seplink [-mode native|split] [-o exe] file.go")
		os.Exit(2)
	}
	input := flag.Arg(0)
	source, err := os.ReadFile(input)
	check(err)
	module, err := goc.CompileExecutableFor(goc.TargetARM64, filepath.Base(input), source)
	check(err)
	objectBytes, assembly, err := arm64.CompileObjectAndAssembly(module)
	check(err)

	work, err := os.MkdirTemp("", "seplink")
	check(err)
	defer os.RemoveAll(work)

	switch *mode {
	case "native":
		check(linkNative(work, objectBytes, assembly, *out))
	case "split":
		check(linkSplit(objectBytes))
	case "pins":
		check(reportPins(objectBytes))
	case "dupsyms":
		check(reportDuplicateSymbols(objectBytes))
	default:
		check(fmt.Errorf("unknown mode %q", *mode))
	}
}

// linkNative assembles the Plan 9 sidecar with cc and then asks cg12's linker,
// rather than cc, to produce the executable.
func linkNative(work string, objectBytes []byte, assembly, out string) error {
	source := filepath.Join(work, "goc-runtime.S")
	sidecar := filepath.Join(work, "goc-runtime.o")
	if err := os.WriteFile(source, []byte(assembly), 0o644); err != nil {
		return err
	}
	assemble := exec.Command("cc", "-c", "-o", sidecar, source)
	assemble.Stderr = os.Stderr
	if err := assemble.Run(); err != nil {
		return fmt.Errorf("assemble sidecar: %w", err)
	}
	sidecarBytes, err := os.ReadFile(sidecar)
	if err != nil {
		return err
	}
	// cg12 lowers a few unreachable-by-construction paths to a call to libc's
	// `abort` (goc/compile.go:962 and three more sites). cc supplies it; a static
	// link with no C runtime does not, so the probe supplies a stub that raises
	// SIGABRT itself. This is the only external symbol goc output needs.
	stubSource := filepath.Join(work, "goc-stubs.S")
	stubObject := filepath.Join(work, "goc-stubs.o")
	if err := os.WriteFile(stubSource, []byte(runtimeStubs), 0o644); err != nil {
		return err
	}
	assembleStub := exec.Command("cc", "-c", "-o", stubObject, stubSource)
	assembleStub.Stderr = os.Stderr
	if err := assembleStub.Run(); err != nil {
		return fmt.Errorf("assemble stubs: %w", err)
	}
	stubBytes, err := os.ReadFile(stubObject)
	if err != nil {
		return err
	}
	linker := link.New()
	if err := linker.AddObjectFile(objectBytes); err != nil {
		return fmt.Errorf("add goc object: %w", err)
	}
	if err := linker.AddObjectFile(sidecarBytes); err != nil {
		return fmt.Errorf("add sidecar object: %w", err)
	}
	if err := linker.AddObjectFile(stubBytes); err != nil {
		return fmt.Errorf("add stubs: %w", err)
	}
	// LinkExecutable would prepend the backend's own `_start`, which calls entry
	// with whatever happens to be in x0/x1. The Go runtime's main wants argc and
	// argv, which on Linux are at [sp] and sp+8 at process entry, so the probe
	// supplies its own entry stub and asks for the image directly.
	merged, err := linker.Link()
	if err != nil {
		return fmt.Errorf("link: %w", err)
	}
	image, err := merged.WriteExecutable("_gocstart")
	if err != nil {
		return fmt.Errorf("write executable: %w", err)
	}
	if out == "" {
		out = "a.out"
	}
	if err := os.WriteFile(out, image, 0o755); err != nil {
		return err
	}
	fmt.Printf("seplink: wrote %s (%d bytes)\n", out, len(image))
	return nil
}

// linkSplit reads the object back, partitions its symbols into two halves at the
// midpoint of .text, and rebuilds each half as a standalone relocatable object
// with the other half's symbols left undefined. Linking them back together
// exercises exactly the cross-object resolution a prebuilt runtime would need.
func linkSplit(objectBytes []byte) error {
	whole, err := obj.ReadELF(objectBytes)
	if err != nil {
		return fmt.Errorf("read object: %w", err)
	}
	fmt.Printf("seplink: whole object: text=%d data=%d syms=%d relocs=%d/%d\n",
		len(whole.Text), len(whole.Data), len(whole.Syms), len(whole.Relocs), len(whole.DataRelocs))

	cut := uint64(len(whole.Text) / 2)
	// Only cut on a symbol boundary, so no function is split down the middle.
	for _, s := range whole.Syms {
		if s.Section == obj.SecText && s.Value >= cut {
			cut = s.Value
			break
		}
	}
	lower, upper := splitText(whole, cut)
	fmt.Printf("seplink: split .text at %d: lower text=%d syms=%d, upper text=%d syms=%d\n",
		cut, len(lower.Text), len(lower.Syms), len(upper.Text), len(upper.Syms))

	lowerBytes, err := lower.MarshalELF()
	if err != nil {
		return fmt.Errorf("marshal lower: %w", err)
	}
	upperBytes, err := upper.MarshalELF()
	if err != nil {
		return fmt.Errorf("marshal upper: %w", err)
	}
	// Control: the same linker over the *unsplit* object. Its .text is what the
	// split link has to reproduce; comparing against the input object instead
	// would only show the intra-.text branches the linker legitimately patches.
	control := link.New()
	if err := control.AddObjectFile(objectBytes); err != nil {
		return fmt.Errorf("add control: %w", err)
	}
	controlMerged, err := control.Link()
	if err != nil {
		return fmt.Errorf("control link: %w", err)
	}

	linker := link.New()
	if err := linker.AddObjectFile(lowerBytes); err != nil {
		return fmt.Errorf("add lower: %w", err)
	}
	if err := linker.AddObjectFile(upperBytes); err != nil {
		return fmt.Errorf("add upper: %w", err)
	}
	merged, err := linker.Link()
	if err != nil {
		return fmt.Errorf("relink: %w", err)
	}
	fmt.Printf("seplink: relinked: text=%d data=%d syms=%d relocs=%d\n",
		len(merged.Text), len(merged.Data), len(merged.Syms), len(merged.Relocs))
	fmt.Printf("seplink: control (whole object through the linker): text=%d relocs=%d\n",
		len(controlMerged.Text), len(controlMerged.Relocs))
	if !bytes.Equal(merged.Text, controlMerged.Text) {
		differences := 0
		first := -1
		for i := range merged.Text {
			if i < len(controlMerged.Text) && merged.Text[i] != controlMerged.Text[i] {
				differences++
				if first < 0 {
					first = i
				}
			}
		}
		fmt.Printf("seplink: split .text differs from the control in %d bytes, first at %#x\n", differences, first)
		return nil
	}
	fmt.Println("seplink: split-and-relinked .text is byte-identical to the control")
	if len(merged.Relocs) != len(controlMerged.Relocs) {
		fmt.Printf("seplink: but the leftover relocation counts differ: %d vs %d\n", len(merged.Relocs), len(controlMerged.Relocs))
	}
	return nil
}

// reportDuplicateSymbols finds names that more than one definition in the same
// object claims. An ELF relocation names its target, and obj's writer maps a name
// to exactly one symbol-table index, so a name with two definitions silently
// resolves every reference to whichever one was written last.
func reportDuplicateSymbols(objectBytes []byte) error {
	whole, err := obj.ReadELF(objectBytes)
	if err != nil {
		return err
	}
	byName := map[string][]obj.Sym{}
	for _, s := range whole.Syms {
		byName[s.Name] = append(byName[s.Name], s)
	}
	references := map[string]int{}
	for _, list := range [][]obj.Reloc{whole.Relocs, whole.DataRelocs, whole.RodataRelocs, whole.RelroRelocs} {
		for _, r := range list {
			references[r.Sym]++
		}
	}
	names := make([]string, 0, len(byName))
	for name, list := range byName {
		if len(list) > 1 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	fmt.Printf("seplink: %d defined symbols, %d names with more than one definition\n", len(whole.Syms), len(names))
	for _, name := range names {
		list := byName[name]
		sizes := map[uint64]bool{}
		bodies := map[string]bool{}
		for _, s := range list {
			sizes[s.Size] = true
			body := sectionOf(whole, s.Section)
			if body != nil && s.Size > 0 && s.Value+s.Size <= uint64(len(body)) {
				bodies[fmt.Sprintf("%x", body[s.Value:s.Value+s.Size])] = true
			}
		}
		fmt.Printf("  %-64s %d definitions, %d distinct sizes %v, %d distinct bodies, %d relocations name it\n",
			name, len(list), len(sizes), sizes, len(bodies), references[name])
	}
	return nil
}

func sectionOf(o *obj.Object, kind obj.SecKind) []byte {
	switch kind {
	case obj.SecText:
		return o.Text
	case obj.SecData:
		return o.Data
	case obj.SecRodata:
		return o.Rodata
	case obj.SecRelro:
		return o.Relro
	}
	return nil
}

// reportPins answers whether a linker could drop unreferenced functions from a
// prebuilt runtime. It walks the reference graph from the entry symbol twice:
// once over every relocation, and once ignoring the relocations that originate
// inside the whole-program Go metadata blob. The gap between the two is what
// linker-level dead stripping could recover only if the metadata were rebuilt
// after stripping rather than taken from the prebuilt object.
func reportPins(objectBytes []byte) error {
	whole, err := obj.ReadELF(objectBytes)
	if err != nil {
		return err
	}
	metadataStart := uint64(len(whole.Data))
	for _, s := range whole.Syms {
		if s.Section == obj.SecData && s.Name == ".goc.go.gcbss" {
			metadataStart = s.Value
		}
	}
	textSymbols := map[string]obj.Sym{}
	for _, s := range whole.Syms {
		if s.Section == obj.SecText && s.Func {
			textSymbols[s.Name] = s
		}
	}
	pinnedByMetadata := map[string]bool{}
	for _, r := range whole.DataRelocs {
		if r.Offset >= metadataStart && textSymbols[r.Sym].Name != "" {
			pinnedByMetadata[r.Sym] = true
		}
	}
	total := 0
	for _, s := range textSymbols {
		total += int(s.Size)
	}
	pinnedBytes := 0
	for name := range pinnedByMetadata {
		pinnedBytes += int(textSymbols[name].Size)
	}
	fmt.Printf("seplink: .data is %d bytes; the Go metadata blob starts at %d (%d bytes, %.1f%%)\n",
		len(whole.Data), metadataStart, len(whole.Data)-int(metadataStart),
		100*float64(len(whole.Data)-int(metadataStart))/float64(len(whole.Data)))
	fmt.Printf("seplink: %d function symbols, %d bytes\n", len(textSymbols), total)
	fmt.Printf("seplink: %d of them (%.1f%%), %d bytes (%.1f%%), are referenced by a relocation inside that blob\n",
		len(pinnedByMetadata), 100*float64(len(pinnedByMetadata))/float64(len(textSymbols)),
		pinnedBytes, 100*float64(pinnedBytes)/float64(total))
	return nil
}

// splitText builds two relocatable objects from one: the lower keeps .text below
// cut and all the data, the upper keeps .text at or above it. Each carries the
// relocations whose site is in its own half; references across the cut become
// undefined symbols the linker has to resolve.
func splitText(whole *obj.Object, cut uint64) (*obj.Object, *obj.Object) {
	lower := &obj.Object{Machine: whole.Machine}
	upper := &obj.Object{Machine: whole.Machine}

	lower.Text = append([]byte(nil), whole.Text[:cut]...)
	upper.Text = append([]byte(nil), whole.Text[cut:]...)
	lower.Data = whole.Data
	lower.Rodata = whole.Rodata
	lower.Relro = whole.Relro
	lower.Tdata = whole.Tdata
	lower.BssSize = whole.BssSize
	lower.TbssSize = whole.TbssSize
	lower.DataAlign = whole.DataAlign
	lower.RodataAlign = whole.RodataAlign
	lower.RelroAlign = whole.RelroAlign
	lower.BssAlign = whole.BssAlign
	lower.TlsAlign = whole.TlsAlign
	lower.DataRelocs = whole.DataRelocs
	lower.RodataRelocs = whole.RodataRelocs
	lower.RelroRelocs = whole.RelroRelocs

	for _, s := range whole.Syms {
		if s.Section != obj.SecText {
			lower.Syms = append(lower.Syms, s)
			continue
		}
		if s.Value < cut {
			// Cross-object references have to be resolvable by name, so every
			// symbol the other half might call becomes global.
			s.Global = true
			lower.Syms = append(lower.Syms, s)
			continue
		}
		s.Value -= cut
		s.Global = true
		upper.Syms = append(upper.Syms, s)
	}
	for _, r := range whole.Relocs {
		if r.Offset < cut {
			lower.Relocs = append(lower.Relocs, r)
			continue
		}
		r.Offset -= cut
		upper.Relocs = append(upper.Relocs, r)
	}
	return lower, upper
}

// runtimeStubs supplies the two things a static, C-runtime-free link needs that
// goc output does not define: `abort`, which cg12 lowers a few
// unreachable-by-construction paths to, and a process entry point that hands the
// Go runtime's main the argc and argv Linux leaves on the stack.
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

func check(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "seplink: %v\n", err)
		os.Exit(1)
	}
}
