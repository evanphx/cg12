package link_test

import (
	"bytes"
	"debug/elf"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/link"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/require"
)

// alignModule puts an odd-sized string in .data ahead of everything, so that
// nothing downstream is aligned by accident: .data ends on an odd byte, and
// anything that merely picks up where it stopped is misaligned.
func alignModule() *ir.Module {
	m := ir.NewModule()
	m.Data = append(m.Data,
		// The 16-aligned datum comes first, so the odd-sized one lands last and
		// .data ends on an odd byte. The other order pads .data out to a multiple
		// of 16 and everything after it is aligned by accident.
		&ir.Data{Name: "init16", Align: 16, Linkage: ir.Linkage{Export: true},
			Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{1, 2}}}}, // .data, 16 bytes
		&ir.Data{Name: "msg", Align: 1, Linkage: ir.Linkage{Export: true},
			Items: []ir.DataItem{{Str: "abcde\x00\x00"}}}, // .data, 7 more: ends odd
		&ir.Data{Name: "word", Align: 4, Linkage: ir.Linkage{Export: true},
			Items: []ir.DataItem{{Zero: 4}}}, // .bss
		&ir.Data{Name: "wide", Align: 16, Linkage: ir.Linkage{Export: true},
			Items: []ir.DataItem{{Zero: 16}}}, // .bss, stricter
	)
	f := m.NewFunc("main", ir.ClsW).Export()
	f.Entry().Ret(f.Entry().Load(ir.ClsW, f.Sym("word", 0)))
	return m
}

func symAddrs(t *testing.T, image []byte) (map[string]uint64, *elf.File) {
	t.Helper()
	ef, err := elf.NewFile(bytes.NewReader(image))
	require.NoError(t, err)
	out := map[string]uint64{}
	// A loadable image carries .dynsym; a relocatable one carries .symtab.
	syms, err := ef.Symbols()
	if err != nil {
		syms, err = ef.DynamicSymbols()
	}
	require.NoError(t, err)
	for _, s := range syms {
		out[s.Name] = s.Value
	}
	require.NotEmpty(t, out, "the image names no symbols")
	return out, ef
}

// A datum is padded to its alignment within its section, which means nothing
// unless the section itself is placed at least that well. .bss was the sharp
// case: it began wherever .data happened to stop, so an odd-sized string left
// every .bss word on an odd address. arm64 tolerates that for an ordinary load
// -- the program runs and the tests pass -- and faults on it for an exclusive.
func TestDataAndBssAreAlignedInTheImage(t *testing.T) {
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(alignModule()))
	exe, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{Needed: []string{"libc.so.6"}, Export: []string{"msg", "word", "wide", "init16"}})
	require.NoError(t, err)

	at, ef := symAddrs(t, exe)
	require.Zero(t, at["word"]%4, "word asked for 4-byte alignment, got %#x", at["word"])
	require.Zero(t, at["wide"]%16, "wide asked for 16-byte alignment, got %#x", at["wide"])
	require.Zero(t, at["init16"]%16, "init16 asked for 16-byte alignment, got %#x", at["init16"])

	// A section header that claims an alignment its address does not meet is
	// self-contradictory, whatever the symbols happen to land on.
	for _, s := range ef.Sections {
		if s.Flags&elf.SHF_ALLOC == 0 || s.Addralign < 2 {
			continue
		}
		require.Zerof(t, s.Addr%s.Addralign, "%s claims %d-byte alignment at %#x", s.Name, s.Addralign, s.Addr)
	}
}

// The alignment has to survive merging: each object's contents are laid out for
// their own alignment, so concatenating at whatever length the previous object
// ended at shifts every datum inside it.
func TestAlignmentSurvivesMerge(t *testing.T) {
	odd := ir.NewModule()
	odd.Data = append(odd.Data, &ir.Data{Name: "odd", Align: 1, Linkage: ir.Linkage{Export: true},
		Items: []ir.DataItem{{Str: "xyz"}}}) // 3 bytes

	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(odd))
	require.NoError(t, l.AddModule(alignModule())) // its data must not shift by 3
	exe, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{Needed: []string{"libc.so.6"}, Export: []string{"msg", "word", "wide", "init16"}})
	require.NoError(t, err)

	at, _ := symAddrs(t, exe)
	require.Zero(t, at["word"]%4, "word, after another object's 3 bytes: %#x", at["word"])
	require.Zero(t, at["wide"]%16, "wide, after another object's 3 bytes: %#x", at["wide"])
	require.Zero(t, at["init16"]%16, "init16, after another object's 3 bytes: %#x", at["init16"])
}

// And the aligned .bss is still really there: the loader has to zero all of it,
// so the segment's memsz must cover the padding as well as the bytes.
func TestAlignedBssIsMappedAndZeroed(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	// main returns the sum of the .bss words, which the loader must have zeroed:
	// the padding the alignment inserted is inside the segment, so memsz has to
	// cover it or the tail of .bss is never mapped at all.
	m := ir.NewModule()
	m.Data = append(m.Data,
		&ir.Data{Name: "msg", Align: 1, Items: []ir.DataItem{{Str: "abcde\x00\x00"}}}, // 7: ends odd
		&ir.Data{Name: "word", Align: 4, Items: []ir.DataItem{{Zero: 4}}},
		&ir.Data{Name: "wide", Align: 16, Items: []ir.DataItem{{Zero: 16}}},
	)
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, e.Load(ir.ClsW, f.Sym("word", 0)), e.Load(ir.ClsW, f.Sym("wide", 0))))

	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(m))
	exe, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{Needed: []string{"libc.so.6"}})
	require.NoError(t, err)
	require.Equal(t, 0, runExe(t, exe), "the padded .bss is mapped and zeroed")
}
