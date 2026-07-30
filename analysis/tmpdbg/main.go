package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/prebuilt"
	"github.com/evanphx/cg12/ir"
)

func index(data []*ir.Data) map[string]*ir.Data {
	m := map[string]*ir.Data{}
	for _, d := range data {
		m[d.Name] = d
	}
	return m
}

func render(d *ir.Data) []string {
	var out []string
	for _, it := range d.Items {
		out = append(out, fmt.Sprintf("sub=%d zero=%d off=%d str=%q sym=%q rel=%q ints=%v", it.Sub, it.Zero, it.Off, it.Str, it.Sym, it.RelativeTo, it.Ints))
	}
	return out
}

func main() {
	rt, err := goc.CompileRuntimeModuleFor(goc.TargetARM64)
	if err != nil {
		panic(err)
	}
	rtData := index(rt.Module.Data)
	src, _ := os.ReadFile(os.Args[1])
	// Compile monolithically so nothing is subtracted.
	mono, err := goc.CompileExecutableFor(goc.TargetARM64, os.Args[1], src)
	if err != nil {
		panic(err)
	}
	monoData := index(mono.Data)
	shown := 0
	for name, m := range monoData {
		if !strings.HasPrefix(name, ".goc.type.") {
			continue
		}
		r := rtData[name]
		if r == nil {
			continue
		}
		a, b := render(r), render(m)
		if fmt.Sprint(a) == fmt.Sprint(b) {
			continue
		}
		shown++
		if shown > 4 {
			continue
		}
		fmt.Printf("=== %s\n", name)
		for i := 0; i < len(a) || i < len(b); i++ {
			x, y := "-", "-"
			if i < len(a) {
				x = a[i]
			}
			if i < len(b) {
				y = b[i]
			}
			mark := "  "
			if x != y {
				mark = "**"
			}
			fmt.Printf("%s [%d] rt : %s\n%s      pg : %s\n", mark, i, x, mark, y)
		}
	}
	fmt.Printf("differing .goc.type descriptors: %d\n", shown)
	_ = prebuilt.Options{}
}
