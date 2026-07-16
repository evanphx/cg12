package link_test

import (
	"bytes"
	"debug/elf"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/link"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// images returns each kind of image we emit, so the properties every image should
// have can be checked against all of them at once.
func images(t *testing.T) map[string][]byte {
	t.Helper()
	build := func(f func(*link.Linker) ([]byte, error)) []byte {
		l := link.NewWith(arm64.Backend{})
		require.NoError(t, l.AddModule(moduleReturns("main", 42)))
		b, err := f(l)
		require.NoError(t, err)
		return b
	}
	return map[string][]byte{
		"static":  build(func(l *link.Linker) ([]byte, error) { return l.LinkExecutable("main") }),
		"dynamic": build(func(l *link.Linker) ([]byte, error) { return l.LinkDynamicExecutable("main", "libc.so.6") }),
		"pie":     build(func(l *link.Linker) ([]byte, error) { return l.LinkPIE("main", "libc.so.6") }),
	}
}

// Every image asks for a non-executable stack. Leaving PT_GNU_STACK out is not
// neutral: the kernel then falls back to an executable stack, so its absence
// silently gives up a hardening the image never opted out of.
func TestImagesRequestNonExecutableStack(t *testing.T) {
	for name, image := range images(t) {
		t.Run(name, func(t *testing.T) {
			f, err := elf.NewFile(bytes.NewReader(image))
			require.NoError(t, err)
			for _, p := range f.Progs {
				if p.Type == elf.PT_GNU_STACK {
					assert.Equal(t, elf.PF_R|elf.PF_W, p.Flags, "readable and writable, but never executable")
					return
				}
			}
			t.Fatal("no PT_GNU_STACK segment: the kernel would allow an executable stack")
		})
	}
}

// Every image carries a build ID naming that exact build, which is how a debugger
// matches a binary to its symbols.
func TestImagesCarryABuildID(t *testing.T) {
	for name, image := range images(t) {
		t.Run(name, func(t *testing.T) {
			f, err := elf.NewFile(bytes.NewReader(image))
			require.NoError(t, err)
			var note *elf.Prog
			for _, p := range f.Progs {
				if p.Type == elf.PT_NOTE {
					note = p
				}
			}
			require.NotNil(t, note, "no PT_NOTE segment")

			b := make([]byte, note.Filesz)
			_, err = note.ReadAt(b, 0)
			require.NoError(t, err)
			// namesz=4 ("GNU\0"), descsz=20, type=3 (NT_GNU_BUILD_ID)
			require.GreaterOrEqual(t, len(b), 16)
			assert.Equal(t, "GNU\x00", string(b[12:16]))
			id := b[16:]
			assert.Len(t, id, 20)
			assert.NotEqual(t, make([]byte, 20), id, "the identifier is filled in, not left zeroed")
		})
	}
}

// The identifier is a hash of the image, so the same input yields the same build
// twice over -- an identifier that changed run to run would be useless for
// matching a binary to anything.
func TestBuildIDIsDeterministic(t *testing.T) {
	build := func() []byte {
		l := link.NewWith(arm64.Backend{})
		require.NoError(t, l.AddModule(moduleReturns("main", 42)))
		exe, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{Needed: []string{"libc.so.6"}})
		require.NoError(t, err)
		return exe
	}
	assert.Equal(t, build(), build(), "relinking the same input reproduces the same image")
}

// Adding the note and stack segments must not disturb what the images do.
func TestImagesStillRunWithNoteAndStack(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	for name, image := range images(t) {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, 42, runExe(t, image))
		})
	}
}
