package goc

import (
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
)

func TestApplyNativeStdlibOverlay(t *testing.T) {
	managed := false
	units := map[string]*sourceUnit{
		"runtime": {
			native: []sourceNativeFile{
				{
					path:   "marker.ssa",
					source: "export function w $runtime_overlayMarker() {\n@start\n\t%active =w asm \"mov %w0, #1\" ((\"=r\", w, %active)) exact\n\tret %active\n}\n",
					entry: stdlibOverlayEntry{
						CallConvention: "aapcs64",
						ManagedFrame:   &managed,
						NoSplit:        true,
					},
				},
			},
		},
	}
	module := ir.NewModule()

	if err := applyNativeStdlibOverlays(module, units); err != nil {
		t.Fatal(err)
	}
	if len(module.Funcs) != 1 {
		t.Fatalf("native functions = %d, want 1", len(module.Funcs))
	}
	function := module.Funcs[0]
	if function.Name != "runtime_overlayMarker" {
		t.Fatalf("native function name = %q, want runtime_overlayMarker", function.Name)
	}
	if function.CallConv != ir.CallConvPlatform {
		t.Fatalf("native function call convention = %v, want AAPCS64", function.CallConv)
	}
	if function.ManagedFrame {
		t.Fatal("native function unexpectedly has a managed frame")
	}
	if !function.NoSplit {
		t.Fatal("native function is not marked nosplit")
	}
	if _, err := arm64.CompileToObject(module); err != nil {
		t.Fatalf("compile native overlay to AArch64 object: %v", err)
	}
}
