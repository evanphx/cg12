package arm64

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// Live-range dump: a debugging aid for the register-allocation visualizer. When
// CG12_DUMP_RANGES names the function being compiled (comma-separated list), the
// allocator's post-colouring picture -- every temp's live interval, whether it
// landed in a register / a spill slot / is rematerialised, and the block layout --
// is appended as one JSON object per line to CG12_DUMP_RANGES_FILE (default
// ./ranges.json). The visualizer renders those intervals as horizontal bars so the
// effect of live-range splitting is visible before/after.

type rangeDumpTemp struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	State   string `json:"state"` // "reg" | "spill" | "remat"
	Reg     string `json:"reg"`
	Slot    int    `json:"slot"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
	Crosses bool   `json:"crosses"`
	GC      bool   `json:"gc"`
	Split   bool   `json:"split"`
	Float   bool   `json:"float"`
}

type rangeDumpBlock struct {
	Name  string  `json:"name"`
	Start int     `json:"start"`
	End   int     `json:"end"`
	Freq  float64 `json:"freq"`
}

type rangeDump struct {
	Func   string           `json:"func"`
	NPos   int              `json:"npos"`
	Blocks []rangeDumpBlock `json:"blocks"`
	Temps  []rangeDumpTemp  `json:"temps"`
}

// maybeDumpRanges writes f's post-allocation live-range picture when
// CG12_DUMP_RANGES names it.
func maybeDumpRanges(f *ir.Func, alloc *allocation, num *numbering, cfg *analysis.CFG, freq *analysis.Freq) {
	want := os.Getenv("CG12_DUMP_RANGES")
	if want == "" || !dumpNameMatch(want, f.Name) {
		return
	}
	d := rangeDump{Func: f.Name, NPos: len(num.posInstr)}
	for _, b := range cfg.RPO {
		p := num.pos[b]
		d.Blocks = append(d.Blocks, rangeDumpBlock{Name: b.Name, Start: p[0], End: p[1], Freq: freq.Of(b)})
	}
	for _, iv := range alloc.intervals {
		t := f.Temps[iv.temp]
		if t == nil {
			continue
		}
		td := rangeDumpTemp{
			ID: iv.temp, Name: t.Name, Start: iv.start, End: iv.end,
			Crosses: iv.crosses, GC: t.GCRef, Float: t.Cls.IsFloat(), Slot: t.Slot,
			Split: strings.HasSuffix(t.Name, ".sr"),
		}
		switch {
		case t.Reg != ir.NoReg:
			td.State, td.Reg = "reg", Reg(t.Reg).Name(8)
		case t.Slot == -1:
			td.State = "remat"
		default:
			td.State = "spill"
		}
		d.Temps = append(d.Temps, td)
	}
	path := os.Getenv("CG12_DUMP_RANGES_FILE")
	if path == "" {
		path = "ranges.json"
	}
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer fh.Close()
	_ = json.NewEncoder(fh).Encode(d)
}

func dumpNameMatch(list, name string) bool {
	for _, n := range strings.Split(list, ",") {
		if strings.TrimSpace(n) == name {
			return true
		}
	}
	return false
}
