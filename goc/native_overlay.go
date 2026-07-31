package goc

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
	cg12parse "github.com/evanphx/cg12/parse"
)

func applyNativeStdlibOverlays(module *ir.Module, units map[string]*sourceUnit) error {
	functionNames := make(map[string]bool, len(module.Funcs))
	for _, function := range module.Funcs {
		functionNames[function.Name] = true
	}
	dataNames := make(map[string]bool, len(module.Data))
	for _, data := range module.Data {
		dataNames[data.Name] = true
	}

	// Import-path order, not map order: each overlay appends its functions and
	// data to the module, so the walk decides where every one of them lands and
	// therefore what address it gets. Only one package carries a native overlay
	// today, which is the only reason ranging the map had nothing to disagree
	// about; a second one would have made the compile irreproducible.
	for _, unit := range orderedUnits(units) {
		packagePath := unit.path
		for _, native := range unit.native {
			overlayModule, err := cg12parse.Parse(native.source)
			if err != nil {
				return fmt.Errorf("parse native standard library overlay %s: %w", native.path, err)
			}
			if len(overlayModule.Types) != 0 {
				return fmt.Errorf("native standard library overlay %s declares aggregate types; cross-module type remapping is not supported yet", native.path)
			}
			if len(overlayModule.Assembly) != 0 {
				return fmt.Errorf("native standard library overlay %s contains source assembly", native.path)
			}

			for _, function := range overlayModule.Funcs {
				if functionNames[function.Name] {
					return fmt.Errorf("native standard library overlay %s duplicates function %s", native.path, function.Name)
				}
				if err := applyNativeOverlayFunctionPolicy(function, packagePath, native.entry); err != nil {
					return fmt.Errorf("native standard library overlay %s: %w", native.path, err)
				}
				functionNames[function.Name] = true
				module.Funcs = append(module.Funcs, function)
			}
			for _, data := range overlayModule.Data {
				if dataNames[data.Name] {
					return fmt.Errorf("native standard library overlay %s duplicates data %s", native.path, data.Name)
				}
				dataNames[data.Name] = true
				module.Data = append(module.Data, data)
			}
		}
	}
	return nil
}

func applyNativeOverlayFunctionPolicy(function *ir.Func, packagePath string, entry stdlibOverlayEntry) error {
	switch entry.CallConvention {
	case "", "aapcs64":
		function.CallConv = ir.CallConvPlatform
	case "go_internal":
		function.CallConv = ir.CallConvGoInternal
	default:
		return fmt.Errorf("invalid call convention %q", entry.CallConvention)
	}
	managedFrame := packagePath == "runtime"
	if entry.ManagedFrame != nil {
		managedFrame = *entry.ManagedFrame
	}
	function.ManagedFrame = managedFrame
	function.NoSplit = entry.NoSplit
	return nil
}
