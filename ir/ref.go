package ir

// RefKind tags what an operand Ref refers to.
type RefKind uint8

const (
	RefNone    RefKind = iota // absent operand (the zero Ref)
	RefTemp                   // an SSA temporary; ID indexes Func.Temps
	RefConst                  // a constant; ID indexes Func.Consts
	RefType                   // an aggregate type; ID indexes Module.Types
	RefSlot                   // a stack slot; ID is a per-function slot number
	RefCallArg                // ABI descriptor emitted between isel and regalloc
	RefMem                    // an addressing mode; ID indexes Func.Mems (target lowering)
	RefReg                    // a physical machine register; ID is the target register number
)

// Ref is a compact, comparable operand reference. It is deliberately a small
// value type (no pointers) so instructions stay cheap to copy and slices of
// them stay cache-friendly, mirroring QBE's packed Ref while remaining plain Go.
type Ref struct {
	Kind RefKind
	ID   uint32
}

// R is the absent/zero reference.
var R = Ref{Kind: RefNone}

// IsNone reports whether the ref is absent.
func (r Ref) IsNone() bool { return r.Kind == RefNone }

// IsTemp reports whether the ref names an SSA temporary.
func (r Ref) IsTemp() bool { return r.Kind == RefTemp }

// IsConst reports whether the ref names a constant.
func (r Ref) IsConst() bool { return r.Kind == RefConst }

// Ref returns a temporary-kind reference to t.
func (t *Temp) Ref() Ref { return Ref{Kind: RefTemp, ID: uint32(t.ID)} }

// tempRef and constRef build refs of the given kind.
func tempRef(id int) Ref  { return Ref{Kind: RefTemp, ID: uint32(id)} }
func constRef(id int) Ref { return Ref{Kind: RefConst, ID: uint32(id)} }
func typeRef(id int) Ref  { return Ref{Kind: RefType, ID: uint32(id)} }
func slotRef(id int) Ref  { return Ref{Kind: RefSlot, ID: uint32(id)} }
