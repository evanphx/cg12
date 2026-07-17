package link_test

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/link"
)

// moduleRodataPointer is `static const char *const words[] = {"hello"}` and a
// main that reads through it: a const object whose contents are an address.
//
// The address is not known until the image is loaded, so someone has to write it
// into a datum whose whole point is that it is never written. That is what
// .data.rel.ro is for.
func moduleRodataPointer() *ir.Module {
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{
		Name:    "hello",
		Align:   1,
		Linkage: ir.Linkage{Section: ".rodata"},
		Items:   []ir.DataItem{{Str: "hello"}, {Sub: ir.SubB, Ints: []int64{0}}},
	})
	m.Data = append(m.Data, &ir.Data{
		Name:    "words",
		Align:   8,
		Linkage: ir.Linkage{Section: ".rodata"},
		Items:   []ir.DataItem{{Sym: "hello"}},
	})
	// An ordinary writable global, which must stay writable. PT_GNU_RELRO freezes
	// whole pages, so .data has to start on a page the relro region does not reach
	// into -- which is arithmetic on the region's size, and getting it wrong makes
	// the store below fault rather than return a wrong answer.
	m.Data = append(m.Data, &ir.Data{
		Name:  "scratch",
		Align: 8,
		Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{0}}},
	})
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Store(f.Long(1), f.Sym("scratch", 0))
	// return strlen(words[0]) + scratch - 1 -- the pointer must have been relocated
	// for this to be 5 rather than a fault on address 0.
	p := e.Load(ir.ClsL, f.Sym("words", 0))
	n := e.Call(ir.ClsW, f.Sym("strlen", 0), p)
	n = e.Add(ir.ClsW, n, e.Load(ir.ClsW, f.Sym("scratch", 0)))
	e.Ret(e.Sub(ir.ClsW, n, f.Word(1)))
	return m
}

// A PIE whose read-only data holds an address links and runs: the pointer is
// relocated at load time, which means the datum was writable while the loader
// worked and read-only by the time the program did.
//
// Before .data.rel.ro this could not link at all -- the relocation had nowhere
// to go, and the linker said so rather than emit an image that would fault.
func TestPIERodataPointerRuns(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("a dynamic arm64 image runs natively only on an arm64 host")
	}
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(moduleRodataPointer()))
	exe, err := l.LinkPIE("main", "libc.so.6")
	require.NoError(t, err)
	require.Equal(t, 5, runExe(t, exe), `strlen(words[0]) where words[0] == "hello"`)
}

// Everything the loader writes into the image must lie inside PT_GNU_RELRO,
// which is the promise the section's name makes: writable while the loader
// works, read-only before the program runs.
//
// This is checked by inspection rather than by running, because running cannot
// see it. glibc rounds the relro region's end DOWN to a page before it mprotects
// -- it must not freeze .data by accident -- so a .data.rel.ro that spills past
// the region simply never gets frozen. The program then behaves correctly and
// the hardening is silently gone, which is the whole failure worth catching.
func TestPIERelroCoversEverythingTheLoaderWrites(t *testing.T) {
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(moduleRodataPointer()))
	image, err := l.LinkPIE("main", "libc.so.6")
	require.NoError(t, err)

	f, err := elf.NewFile(bytes.NewReader(image))
	require.NoError(t, err)

	var relro *elf.Prog
	for _, p := range f.Progs {
		if p.Type == elf.PT_GNU_RELRO {
			relro = p
		}
	}
	require.NotNil(t, relro, "a PIE with const pointer data has a relro region")

	const page = 0x1000
	require.Zero(t, (relro.Vaddr+relro.Memsz)%page,
		"the region must end on a page: the loader freezes whole pages and rounds this end down, "+
			"so anything past the last boundary is quietly left writable")

	// Every RELATIVE relocation -- each address the loader computes and stores --
	// must land in the frozen region.
	rela := f.Section(".rela.dyn")
	require.NotNil(t, rela)
	raw, err := rela.Data()
	require.NoError(t, err)
	const relative = 1027 // R_AARCH64_RELATIVE
	n := 0
	for off := 0; off+24 <= len(raw); off += 24 {
		at := binary.LittleEndian.Uint64(raw[off:])
		if binary.LittleEndian.Uint64(raw[off+8:])&0xffffffff != relative {
			continue
		}
		n++
		require.GreaterOrEqualf(t, at, relro.Vaddr,
			"a relative relocation writes %#x, before the relro region at %#x", at, relro.Vaddr)
		require.Lessf(t, at, relro.Vaddr+relro.Memsz,
			"a relative relocation writes %#x, past the relro region ending %#x",
			at, relro.Vaddr+relro.Memsz)
	}
	require.NotZero(t, n, "this program has a const pointer, so the loader has something to write")
}

// The same data in a *static* image, which has no loader at all. The bytes still
// have to be placed and the pointer still has to be written -- by the linker,
// now, since every address is a link-time constant.
//
// The static writer knowing nothing about a section the dynamic writer handles is
// how .bss was broken before it (see TestStaticExecutablePlacesBss): the symbol
// simply never gets an address. So this checks the same program end to end
// without libc: main returns the first byte of the string it reaches through the
// const pointer.
func TestStaticExecutableRodataPointerRuns(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	m := ir.NewModule()
	m.Data = append(m.Data,
		&ir.Data{
			Name: "letter", Align: 1,
			Linkage: ir.Linkage{Section: ".rodata"},
			Items:   []ir.DataItem{{Sub: ir.SubB, Ints: []int64{7}}},
		},
		&ir.Data{
			Name: "letterp", Align: 8,
			Linkage: ir.Linkage{Section: ".rodata"},
			Items:   []ir.DataItem{{Sym: "letter"}},
		},
	)
	// exit(*letterp) -- 7 only if the pointer was written and points at `letter`.
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	p := e.Load(ir.ClsL, f.Sym("letterp", 0))
	e.Ret(e.LoadSub(ir.ClsW, ir.SubUB, p))

	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(m))
	exe, err := l.LinkExecutable("main")
	require.NoError(t, err, "a static executable with const pointer data must link")
	require.Equal(t, 7, runExe(t, exe), "*letterp == 7: the pointer was resolved at link time")
}

// The same program as a fixed-base executable. Nothing here needs the loader --
// every address is a link-time constant -- so this passed before .data.rel.ro
// existed and must keep passing: the new section must not capture data that has
// no reason to leave .rodata.
func TestFixedBaseRodataPointerRuns(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("a dynamic arm64 image runs natively only on an arm64 host")
	}
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(moduleRodataPointer()))
	exe, err := l.LinkDynamicExecutable("main", "libc.so.6")
	require.NoError(t, err)
	require.Equal(t, 5, runExe(t, exe))
}

// The amd64 backend routes const pointer data to .data.rel.ro too. That routing
// is per-backend code -- each has its own addData -- so a copy that kept sending
// it to .rodata would fail to link, and one that sent it to .data would link and
// quietly stay writable. Only running the thing on the target says which.
func TestAmd64PIERodataPointerRuns(t *testing.T) {
	l := link.NewWith(amd64.Backend{})
	require.NoError(t, l.AddModule(moduleRodataPointer()))
	exe, err := l.LinkPIE("main", "libc.so.6")
	require.NoError(t, err)
	require.Equal(t, 5, runX86Dyn(t, exe), `strlen(words[0]) where words[0] == "hello"`)
}
