package arm64

import (
	"sync"
	"sync/atomic"

	"github.com/evanphx/cg12/ir"
)

// The back end's half of BUILD_CACHE.md's Option C memo.
//
// Lowering, register allocation and emission read one function and read-only
// module facts -- that is the property the parallel backend already relies on,
// and TestParallelBackendIsByteIdenticalToSerial holds it to it. So the finished
// code for a function is a pure function of its finished body, and the back-end
// memo needs no dependency set at all: the body's digest is the whole key. That
// is why this half is content-addressed while the optimiser half is keyed on the
// inliner's recorded set.
//
// Everything the object needs from a compiled function is copied out of
// functionCode at merge time -- code, relocations, block symbols, line rows,
// DWARF, Go metadata, stack maps and the nosplit budget's facts -- and a large
// module then drops the function's IR outright (releaseFunctionIR). So a cached
// functionCode is a complete substitute for having compiled it, not a partial
// one.
//
// The cache is process-global for the same reason opt's dependency recorder is:
// the alternative is threading it through compileToObjectWithBundle,
// compileFunctionsInOrder and compileFunction, none of which otherwise know
// anything about caching. It is nil unless a caller installs one.
var (
	codeCacheMu   sync.Mutex
	codeCacheKey  func(*ir.Func) ([32]byte, bool)
	codeCache     map[[32]byte]*functionCode
	codeCacheHits atomic.Int64
)

// SetFunctionCodeCache installs a per-function code cache. key returns the
// content key for a function and whether it has one; a function with no key
// (an assembly-derived one, say) is compiled normally and not cached.
//
// Passing nil removes the cache and releases what it held.
func SetFunctionCodeCache(key func(*ir.Func) ([32]byte, bool)) {
	codeCacheMu.Lock()
	defer codeCacheMu.Unlock()
	codeCacheKey = key
	codeCacheHits.Store(0)
	if key == nil {
		codeCache = nil
		return
	}
	if codeCache == nil {
		codeCache = map[[32]byte]*functionCode{}
	}
}

// FunctionCodeCacheHits reports how many functions the cache answered since it
// was installed.
func FunctionCodeCacheHits() int { return int(codeCacheHits.Load()) }

// FunctionCodeCacheBytes reports what the cache is holding: the code, the
// relocations and the metadata, counted as they would be written to disk. It is
// the honest answer to "what does the back-end half of the memo cost".
func FunctionCodeCacheBytes() int64 {
	codeCacheMu.Lock()
	defer codeCacheMu.Unlock()
	var total int64
	for _, code := range codeCache {
		total += code.bytes()
	}
	return total
}

// cachedFunctionCode returns the cached code for f if there is any, the content
// key to file it under, and whether f has a key at all. A function with no key
// -- an assembly-derived one, say -- is compiled normally and not cached, which
// is why "keyed" is a separate answer from "hit": without it every keyless
// function would collide on the zero key.
func cachedFunctionCode(f *ir.Func) (code *functionCode, key [32]byte, keyed bool) {
	codeCacheMu.Lock()
	defer codeCacheMu.Unlock()
	if codeCacheKey == nil {
		return nil, key, false
	}
	key, keyed = codeCacheKey(f)
	if !keyed {
		return nil, key, false
	}
	if hit, ok := codeCache[key]; ok {
		codeCacheHits.Add(1)
		return hit, key, true
	}
	return nil, key, true
}

func storeFunctionCode(key [32]byte, code *functionCode) {
	codeCacheMu.Lock()
	defer codeCacheMu.Unlock()
	if codeCache == nil {
		return
	}
	codeCache[key] = code
}

// bytes is what one entry would occupy serialized: the code plus every table the
// merge copies out of it, counted field by field rather than estimated.
func (c *functionCode) bytes() int64 {
	total := int64(len(c.name)) + int64(len(c.code)) + 8
	total += int64(len(c.relocs)) * (8 + 4 + 8)
	for _, r := range c.relocs {
		total += int64(len(r.Sym))
	}
	for _, b := range c.blockSyms {
		total += int64(len(b.name)) + 8
	}
	total += int64(len(c.rows)) * 32
	total += int64(len(c.dwarf.Name)+len(c.dwarf.Sym)) + 48
	total += int64(len(c.dwarf.Params)) * 32
	total += int64(len(c.goFunction.Name)) + 64
	total += int64(len(c.goFunction.LocalPointerWords)+
		len(c.goFunction.ArgumentPointerWords)+
		len(c.goFunction.SafepointArgumentPointerWords)) * 8
	for _, p := range c.goFunction.StackMapPoints {
		total += 16 + int64(len(p.PointerWords))*8
	}
	if c.stackMap != nil {
		total += int64(len(c.stackMap.sym))
		for _, p := range c.stackMap.points {
			total += 16 + int64(len(p.roots))*16
		}
	}
	total += int64(len(c.stack.name)) + 24
	for _, call := range c.stack.calls {
		total += int64(len(call))
	}
	for _, taken := range c.stack.addressTaken {
		total += int64(len(taken))
	}
	return total
}
