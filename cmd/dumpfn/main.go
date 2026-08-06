package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/opt"

	_ "github.com/evanphx/cg12/arm64"
)

func main() {
	src, _ := os.ReadFile(os.Args[1])
	m, err := goc.CompileExecutableFor(goc.TargetARM64, filepath.Base(os.Args[1]), src)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	p := opt.DefaultPipeline()
	opt.Run(m, p[:opt.PerFunctionPrefixLen])
	for _, f := range m.Funcs {
		for _, want := range os.Args[2:] {
			if f.Name == want || strings.Contains(f.Name, want) {
				fmt.Println(f.String())
			}
		}
	}
}
