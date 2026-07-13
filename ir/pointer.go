package ir

// LowerPointers resolves the abstract pointer class [ClsP] to ptr, the target's
// concrete word-register class (ClsL on a 64-bit machine, ClsW on wasm32). It is
// the single point that couples pointer width to register width: every pointer
// value becomes an ordinary integer of the register class, and every pointer
// load/store is narrowed to that same width. Because one class drives both the
// value representation and the memory width, the two cannot diverge.
//
// A backend calls this once, before any lowering, so the rest of the backend
// never sees a ClsP. Genuine long (ClsL) values are untouched: only values the
// front end deliberately typed as pointers move.
func LowerPointers(f *Func, ptr Cls) {
	loadOp, storeOp := ptrMemOps(ptr)

	// Narrow pointer loads/stores first, while classes still say ClsP. A
	// pointer load is a full load whose result class is ClsP; a pointer store
	// is a full store whose value operand is a ClsP.
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			switch {
			case in.Op == OLoadl && in.Cls == ClsP:
				in.Op = loadOp
			case in.Op == OStorel && f.ClassOf(in.Arg(0)) == ClsP:
				in.Op = storeOp
			}
		}
	}

	// A function may itself return a pointer.
	if f.Retty == ClsP {
		f.Retty = ptr
	}

	// Rewrite every ClsP occurrence to the concrete register class. Parameters
	// are temporaries, so this covers a pointer-typed parameter too.
	for _, t := range f.Temps {
		if t != nil && t.Cls == ClsP {
			t.Cls = ptr
		}
	}
	for i := range f.Consts {
		if f.Consts[i].Cls == ClsP {
			f.Consts[i].Cls = ptr
		}
	}
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			if b.Instrs[i].Cls == ClsP {
				b.Instrs[i].Cls = ptr
			}
		}
		for _, p := range b.Phis {
			if p.Cls == ClsP {
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
