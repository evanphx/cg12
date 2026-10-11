package goc_test

import (
	"fmt"
	"io"
	"runtime"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
)

// goRuntimeExecutionSupported reports whether this host can build a
// cg12-compiled Go executable -- the whole runtime plus its translated Plan 9
// assembly -- and run it natively.
func goRuntimeExecutionSupported() bool {
	return runtime.GOOS == "linux" && (runtime.GOARCH == "arm64" || runtime.GOARCH == "amd64")
}

// goRuntimeExecutionSkip is the skip message for a host that cannot.
const goRuntimeExecutionSkip = "Go runtime execution test requires Linux ARM64 or AMD64"

// writeNativeObjectAndAssembly runs the host architecture's backend over a Go
// module, streaming the object to w and returning the translated assembly
// sidecar that has to be assembled and linked alongside it.
func writeNativeObjectAndAssembly(w io.Writer, module *ir.Module) (string, error) {
	switch runtime.GOARCH {
	case "arm64":
		return arm64.WriteObjectAndAssembly(w, module)
	case "amd64":
		return amd64.WriteObjectAndAssembly(w, module)
	}
	return "", fmt.Errorf("no backend for %s", runtime.GOARCH)
}
