package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/prebuilt"
	"github.com/evanphx/cg12/internal/runtimepack"
)

func main() {
	iterations, err := strconv.Atoi(os.Args[1])
	if err != nil {
		panic(err)
	}
	packPath := os.Args[2]
	packStart := time.Now()
	pack, err := runtimepack.Read(packPath)
	if err != nil {
		panic(err)
	}
	fmt.Printf("pack read: %.3fs\n", time.Since(packStart).Seconds())
	for _, path := range os.Args[3:] {
		src, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		for i := 0; i < iterations; i++ {
			t := time.Now()
			_, err := prebuilt.CompileProgram(goc.TargetARM64, path, src, pack, prebuilt.Options{})
			if err != nil {
				panic(err)
			}
			fmt.Printf("%-40s iter=%d %.3fs\n", path, i, time.Since(t).Seconds())
		}
	}
}
