//go:build !linux || !cgo

package libbpfload

// Load is unavailable without cgo on Linux; it reports the object cannot be
// checked here.
func Load(elf []byte) int { return -1000 }

// Available reports whether the real libbpf loader is compiled in.
func Available() bool { return false }
