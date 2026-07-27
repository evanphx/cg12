package ir

// LowerPointers resolves the abstract pointer classes [ClsP] and [ClsM] to ptr,
// the target's concrete word-register class (ClsL on a 64-bit machine, ClsW on
// wasm32). It is the single point that couples pointer width to register width:
// every pointer value becomes an ordinary integer of the register class, and
// every pointer load/store is narrowed to that same width. Because one class
// drives both the value representation and the memory width, the two cannot
// diverge.
//
// A managed pointer ([ClsM]) resolves to the same register class but additionally
// marks its defining temporary a GC reference, so the value-level managed-ness the
// class carried survives into the backend as [Temp.GCRef].
//
// A backend calls this once, before any lowering, so the rest of the backend
// never sees a ClsP or ClsM. Genuine long (ClsL) values are untouched: only values
// the front end deliberately typed as pointers move.
func LowerPointers(f *Func, ptr Cls) {
	loadOp, storeOp := ptrMemOps(ptr)

	// Narrow pointer loads/stores first, while classes still say ClsP/ClsM. A
	// pointer load is a full load whose result class is a pointer; a pointer store
	// is a full store whose value operand is a pointer.
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			switch {
			case in.Op == OLoadl && in.Cls.IsPtr():
				in.Op = loadOp
			case in.Op == OStorel && f.ClassOf(in.Arg(0)).IsPtr():
				in.Op = storeOp
			}
		}
	}

	// A function may itself return a pointer.
	if f.Retty.IsPtr() {
		f.Retty = ptr
	}

	// Rewrite every pointer occurrence to the concrete register class. Parameters
	// are temporaries, so this covers a pointer-typed parameter too. A managed
	// temporary becomes a GC reference so the backend still scans it as a root.
	for _, t := range f.Temps {
		if t == nil {
			continue
		}
		if t.Cls == ClsM {
			t.GCRef = true
		}
		if t.Cls.IsPtr() {
			t.Cls = ptr
		}
	}
	for i := range f.Consts {
		if f.Consts[i].Cls.IsPtr() {
			f.Consts[i].Cls = ptr
		}
	}
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			if b.Instrs[i].Cls.IsPtr() {
				b.Instrs[i].Cls = ptr
			}
		}
		for _, p := range b.Phis {
			if p.Cls.IsPtr() {
				p.Cls = ptr
			}
		}
	}
}

// ptrMemOps returns the full load and store opcodes whose memory width matches
// the pointer class ptr, so a stored pointer occupies exactly ptr.Size() bytes.
func ptrMemOps(ptr Cls) (load, store Op) {
	if ptr == ClsW {
		return OLoaduw, OStorew // 32-bit pointers: 4-byte memory image
	}
	return OLoadl, OStorel // 64-bit pointers: 8-byte memory image
}
