package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/evanphx/cg12/internal/permodule"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A goc image used to be able to hold exactly one Go module. These tests link a
// real goc-compiled program together with a second, separately compiled Go
// module and run the result.
//
// The second module's 32-bit NameOff/TypeOff values were baked by the back end
// with no relocation left behind, against a base that sits at offset 0 of its own
// object and megabytes into the merged image. They stay correct because the
// module carries a moduledata of its own whose types/etypes bound its own data,
// and runtime.resolveNameOff/resolveTypeOff pick the module containing the
// referring pointer. Every control below removes one half of that and must fail.

const permoduleProbe = "testdata/permodule_probe.go"

// buildTwoModuleImage links the probe program with a second Go module and
// returns the path of the executable.
func buildTwoModuleImage(t *testing.T, options permodule.ImageOptions) string {
	t.Helper()
	if runtime.GOARCH != "arm64" {
		t.Skip("linux/arm64 Go runtime image")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc is required to assemble the Go runtime's Plan 9 sidecar")
	}
	options.SourcePath = permoduleProbe
	options.Report = func(line string) { t.Log("permodule:", line) }
	image, err := permodule.BuildImage(options)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "two-module")
	require.NoError(t, os.WriteFile(path, image, 0o755))
	return path
}

// runImage runs the executable and returns its combined output and exit status.
func runImage(t *testing.T, path string, environment ...string) (string, int) {
	t.Helper()
	command := exec.Command(path)
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return string(output), exit.ExitCode()
	}
	require.NoError(t, err, "image output:\n%s", output)
	return string(output), 0
}

// The whole mechanism, on real goc output. Each assertion names the piece it
// covers.
func TestGoImageCarriesASecondModule(t *testing.T) {
	image := buildTwoModuleImage(t, permodule.ImageOptions{Mode: permodule.ModePerModule})
	output, status := runImage(t, image)
	require.Equal(t, 0, status, "image output:\n%s", output)
	t.Log(output)

	// The second module's own type descriptors, read through NameOff.
	assert.Contains(t, output, "foreign-int:int")
	assert.Contains(t, output, "foreign-int-kind:2")

	// moduledata.typelinks: PtrToThis is a TypeOff into the second module, and
	// the typemap runtime.typelinksinit built turns it into the *program*
	// module's *int. Two descriptors, one Go type, one identity.
	assert.Contains(t, output, "foreign-ptr:*int")
	assert.Contains(t, output, "ptr-identity: same")

	// The second module's own pclntab: runtime.FuncForPC found the module and
	// named the function. That function is at text offset 0 of its module, the
	// slot whose name the runtime used to read as the empty string.
	assert.Contains(t, output, "first-func:_goc_probe_entry")
	assert.Contains(t, output, "first-call:7")

	// A traceback that walks a frame only the second module's pcsp table
	// describes.
	assert.Contains(t, output, "frame:_goc_probe_hold")

	// And a GC stack scan over that frame: the payload's only live reference is
	// the second module's locals stack map.
	assert.Contains(t, output, "payload: intact")
}

// The offsets do not depend on where the second module lands, which is the
// property the whole design is about. Padding shifts its base by an arbitrary
// amount and nothing about the run changes.
func TestSecondModuleOffsetsAreIndependentOfItsPlacement(t *testing.T) {
	var outputs []string
	for _, pad := range []int{0, 8, 4096, 100001} {
		image := buildTwoModuleImage(t, permodule.ImageOptions{Mode: permodule.ModePerModule, Pad: pad})
		output, status := runImage(t, image)
		require.Equalf(t, 0, status, "pad=%d output:\n%s", pad, output)
		outputs = append(outputs, output)
	}
	for index := 1; index < len(outputs); index++ {
		assert.Equal(t, outputs[0], outputs[index], "the run must not depend on where the second module lands")
	}
}

// The GC stack scan, named exactly. GODEBUG=cg12scanroots prints every pointer
// word the precise scan retains, with the frame it came from; filtering it for
// the payload's first word shows which frames keep the object alive.
//
// If the second module's locals stack map were missing or wrong, no frame would
// name the object and it would be collected.
func TestSecondModuleFrameIsTheOnlyGCRootOfItsPayload(t *testing.T) {
	image := buildTwoModuleImage(t, permodule.ImageOptions{Mode: permodule.ModePerModule})
	output, status := runImage(t, image, "GOMAXPROCS=1", "GODEBUG=cg12scanroots=1")
	require.Equal(t, 0, status, "image output:\n%s", output)

	var retaining []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "cg12scanroots: ") && strings.Contains(line, "head 0x5ea1ed") {
			retaining = append(retaining, line)
		}
	}
	require.NotEmpty(t, retaining, "no frame retained the payload; output:\n%s", output)
	for _, line := range retaining {
		assert.Contains(t, line, "_goc_probe_hold local slot 0",
			"the payload must be retained by the second module's frame and nothing else")
	}
	assert.Contains(t, output, "payload: intact")
}

// Control for the type region. With one flat region spanning both objects --
// which is what cg12 emitted before -- the second module's baked offsets are
// read against the program's base and land somewhere else entirely.
func TestFlatTypeRegionFailsOnASecondModule(t *testing.T) {
	image := buildTwoModuleImage(t, permodule.ImageOptions{Mode: permodule.ModeFlat})
	output, status := runImage(t, image)
	assert.NotEqual(t, 0, status, "a single flat type region must not survive a second module; output:\n%s", output)
	assert.NotContains(t, output, "probe: done")
}

// Control for the text-end symbol. internal/gometa's text-end symbol used to be
// one image-wide constant defined by the Plan 9 sidecar, so a second module took
// the first module's text end as its own maxpc. runtime.moduledataverify1
// catches it.
func TestSharedTextEndSymbolFailsOnASecondModule(t *testing.T) {
	image := buildTwoModuleImage(t, permodule.ImageOptions{Mode: permodule.ModeSharedTextEnd})
	output, status := runImage(t, image)
	assert.NotEqual(t, 0, status, "a shared text-end symbol must not survive a second module; output:\n%s", output)
	assert.Contains(t, output, "minpc or maxpc invalid")
}

// Control for typelinks. Without them runtime.typelinksinit has nothing to
// canonicalise, so the two modules' descriptions of one Go type stay two
// distinct types and reflect disagrees about identity.
func TestWithoutTypelinksTheSameGoTypeHasTwoIdentities(t *testing.T) {
	image := buildTwoModuleImage(t, permodule.ImageOptions{
		Mode:             permodule.ModePerModule,
		WithoutTypeLinks: true,
	})
	output, status := runImage(t, image)
	require.Equal(t, 0, status, "image output:\n%s", output)
	assert.Contains(t, output, "ptr-identity: different")
	// Everything that does not depend on cross-module type identity still works,
	// which is what makes this a control for typelinks specifically.
	assert.Contains(t, output, "foreign-ptr:*int")
	assert.Contains(t, output, "probe: done")
}
