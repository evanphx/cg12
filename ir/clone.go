package ir

// CloneFunc returns a deep copy of f: a function with the same code, the same
// temporaries, constants and aggregate types, and no structure shared with the
// original beyond the interned type descriptors the encoding resolves by index.
//
// It goes through the binary unit format rather than a hand-written walk. A
// hand-written copy of Func is a maintenance hazard of the worst kind -- it is
// correct until someone adds a field, and then it is silently wrong in whatever
// depends on that field -- while the encoder is already exercised by every
// cached compile in the tree and already has to know every field that matters.
// The cost is one marshal and one unmarshal of a single function, which is what
// buys the caller the ability to try a transformation and put the function back.
//
// The returned function is detached: its Module is the scratch module the decode
// produced, so use [Func.ReplaceBodyFrom] to put a clone back into a module
// rather than substituting the pointer.
func CloneFunc(f *Func) (*Func, error) {
	scratch := &Module{Funcs: []*Func{f}}
	if f.mod != nil {
		// Source positions are indices into the module's file table. Carrying the
		// table keeps a decoded clone printable, and keeps the indices meaning the
		// same thing on both sides of the round trip.
		scratch.Files = f.mod.Files
	}
	encoded, err := scratch.MarshalBinary()
	if err != nil {
		return nil, err
	}
	decoded, err := DecodeModule(encoded)
	if err != nil {
		return nil, err
	}
	return decoded.Funcs[0], nil
}

// ReplaceBodyFrom replaces everything about f that describes its code -- blocks,
// temporaries, constants, parameters, aggregate values and the builder state
// that indexes them -- with other's, and leaves f's identity alone: it keeps its
// name, its linkage, and the module that owns it.
//
// It is how a caller undoes a transformation it measured and rejected. Putting
// the clone into Module.Funcs instead would work for the call graph, which
// resolves callees by symbol name, but would leave a function whose Module is a
// scratch module -- and the module is where source file names and type names are
// resolved from.
func (f *Func) ReplaceBodyFrom(other *Func) {
	owner, name, linkage := f.mod, f.Name, f.Linkage
	*f = *other
	f.mod, f.Name, f.Linkage = owner, name, linkage
}
