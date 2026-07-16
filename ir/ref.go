package ir

// RefKind tags what an operand Ref refers to.
type RefKind uint8

const (
	RefNone  RefKind = iota // absent operand (the zero Ref)
	RefTemp                 // an SSA temporary; ID indexes Func.Temps
	RefConst                // a constant; ID indexes Func.Consts
	RefType                 // an aggregate type; ID indexes Module.Types
	RefSlot                 // a stack slot; ID is a per-function slot number

	// Two kinds were declared here and never used by anything: an ABI descriptor
	// for the gap between isel and regalloc, and an addressing mode indexing a
	// Func.Mems that was never added. They are burned rather than reclaimed
	// because a Ref's kind is written to the binary encoding as its number, so
	// renumbering RefReg would make every previously encoded module decode as
	// something else -- silently, since the decoder would find a kind it knows.
	_ // was RefCallArg
	_ // was RefMem

	RefReg // a physical machine register; ID is the target register number

	// RefAggregate is a frontend-only immutable bundle of scalar SSA refs. It
	// must be consumed by aggregate-aware builders before target lowering.
	RefAggregate
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
