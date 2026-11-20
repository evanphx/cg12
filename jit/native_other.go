//go:build !(linux && (amd64 || arm64))

package jit

import (
	"errors"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// Supported reports whether this platform can run compiled code.
const Supported = false

// Code is unused where compiled code can't run.
type Code struct{}

func (c *Code) Addr(name string) uintptr { return 0 }
func (c *Code) Free()                    {}

// Load fails where compiled code can't run.
func Load(o *obj.Object, resolve func(string) (uintptr, bool)) (*Code, error) {
	return nil, errors.New("jit: unsupported platform")
}

// Call can't be reached where Load always fails.
func Call(fn, a, b, c uintptr) uint64 { panic("jit: unsupported platform") }

// Compile fails where compiled code can't run.
func Compile(m *ir.Module) (*obj.Object, error) { return nil, errors.New("jit: unsupported platform") }
