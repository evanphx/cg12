package ir

// Cls is the class (base type) of an SSA value. Every temporary and constant
// has exactly one class. QBE calls this "cls"; it is the fundamental type over
// which most operations are polymorphic.
type Cls uint8

const (
	ClsW Cls = iota // 32-bit integer ("word")
	ClsL            // 64-bit integer ("long")
	ClsS            // 32-bit IEEE float ("single")
	ClsD            // 64-bit IEEE float ("double")

	// ClsP is the abstract pointer class. Its concrete width is target-defined
	// and is always exactly the target's word-register class: a pointer is an
	// integer that fits a general register, so pointer size and register size
	// cannot diverge. Each backend resolves ClsP to that class up front with
	// [LowerPointers] (arm64 -> ClsL, wasm32 -> ClsW), after which no ClsP
	// remains. Sizing/lowering must therefore never see a live ClsP.
	ClsP

	// ClsQ is a 128-bit IEEE quad ("long double" on AArch64). It has no hardware
	// arithmetic; the front end lowers every quad operation to a libgcc soft-float
	// call. The value occupies a full SIMD (V) register or a 16-byte memory slot.
	ClsQ
)

// IsInt reports whether the class is integer-valued (W, L, or a pointer).
func (c Cls) IsInt() bool { return c == ClsW || c == ClsL || c == ClsP }

// IsFloat reports whether the class is a floating-point class (S, D, or Q) — one
// that lives in a SIMD/FP register.
func (c Cls) IsFloat() bool { return c == ClsS || c == ClsD || c == ClsQ }

// IsPtr reports whether the class is the abstract pointer class.
func (c Cls) IsPtr() bool { return c == ClsP }

// Size returns the width of a value of this class in bytes. ClsP has no width
// of its own — a target's pointer size is exactly its resolved word-register
// class's size (see [LowerPointers]); the fallback here is the widest pointer
// so an accidental pre-resolution query never under-allocates.
func (c Cls) Size() int {
	switch c {
	case ClsW, ClsS:
		return 4
	case ClsL, ClsD, ClsP:
		return 8
	case ClsQ:
		return 16
	}
	return 0
}

// String returns the single-character IL mnemonic for the class.
func (c Cls) String() string {
	switch c {
	case ClsW:
		return "w"
	case ClsL:
		return "l"
	case ClsS:
		return "s"
	case ClsD:
		return "d"
	case ClsP:
		return "p"
	case ClsQ:
		return "q"
	}
	return "?"
}

// SubCls is an extended type used only in a handful of positions where a value
// narrower than a full class matters: aggregate fields, function parameters,
// and the memory width of loads/stores. QBE calls these "sub-word" types.
type SubCls uint8

const (
	SubB  SubCls = iota // signed byte
	SubUB               // unsigned byte
	SubH                // signed halfword (16-bit)
	SubUH               // unsigned halfword
	SubW                // word
	SubL                // long
	SubS                // single
	SubD                // double
	SubQ                // quad (128-bit)
)

// Cls returns the register class a sub-class is materialised into.
func (s SubCls) Cls() Cls {
	switch s {
	case SubS:
		return ClsS
	case SubD:
		return ClsD
	case SubQ:
		return ClsQ
	case SubL:
		return ClsL
	default:
		return ClsW
	}
}

// Size returns the in-memory width of the sub-class in bytes.
func (s SubCls) Size() int {
	switch s {
	case SubB, SubUB:
		return 1
	case SubH, SubUH:
		return 2
	case SubW, SubS:
		return 4
	case SubL, SubD:
		return 8
	case SubQ:
		return 16
	}
	return 0
}

// ElemString returns the unsigned element-type mnemonic used in data
// definitions and aggregate type fields (b/h/w/l/s/d), where signedness is not
// expressed.
func (s SubCls) ElemString() string {
	switch s {
	case SubB, SubUB:
		return "b"
	case SubH, SubUH:
		return "h"
	case SubL:
		return "l"
	case SubS:
		return "s"
	case SubD:
		return "d"
	case SubQ:
		return "q"
	default:
		return "w"
	}
}

// String returns the IL mnemonic for the sub-class.
func (s SubCls) String() string {
	switch s {
	case SubB:
		return "sb"
	case SubUB:
		return "ub"
	case SubH:
		return "sh"
	case SubUH:
		return "uh"
	case SubW:
		return "w"
	case SubL:
		return "l"
	case SubS:
		return "s"
	case SubD:
		return "d"
	case SubQ:
		return "q"
	}
	return "?"
}

// AggType is a user-defined aggregate type (struct, union, or opaque), referred
// to by an instruction's type operand (chiefly calls and typed allocations).
type AggType struct {
	Name   string
	Align  int  // explicit alignment in bytes; 0 means natural
	Size   int  // total size in bytes (opaque types only)
	Opaque bool // opaque type: only Size/Align are known
	Packed bool // __attribute__((packed)): members are placed with no padding
	Fields []Field
	Union  bool      // union: Cases overlap (Fields unused)
	Cases  [][]Field // one field list per union case
}

// Field is one member of an aggregate type. Count > 1 denotes an inline array.
type Field struct {
	Sub   SubCls
	Type  *AggType // non-nil for nested aggregate fields (Sub ignored)
	Count int
}

// count returns the element count of a field (1 unless it is an inline array).
func (f Field) count() int {
	if f.Count > 1 {
		return f.Count
	}
	return 1
}

// sizeAlign returns the size and alignment of a single field element.
func (f Field) sizeAlign() (size, align int) {
	if f.Type != nil {
		return f.Type.Layout()
	}
	s := f.Sub.Size()
	return s, s
}

// Leaf is one scalar element of an aggregate: its type, and its byte offset from
// the start of the aggregate.
type Leaf struct {
	Sub SubCls
	Off int
}

// Layout returns the total size and alignment of an aggregate, following the C
// struct/union layout rules (fields placed at their natural alignment, the whole
// rounded up to the aggregate's alignment; a union is the largest of its cases).
// Opaque types report their declared size and alignment.
func (t *AggType) Layout() (size, align int) {
	size, align, _ = t.walk(0, nil)
	return size, align
}

// Leaves returns every scalar element of an aggregate, each at its byte offset,
// in the order they are laid out.
//
// This is the single answer to where a field is. An ABI classifier needs
// per-leaf offsets and Layout gives only a total, so each one used to walk the
// fields again and apply the placement rule itself -- three copies of it, two
// backends, and nothing keeping them in step.
//
// A union's cases all begin at the same offset, so their leaves overlap: that is
// what a union is, and what a classifier has to see. simple reports that no
// union or opaque type was involved, for a caller that cannot answer its
// question about one.
func (t *AggType) Leaves() (leaves []Leaf, simple bool) {
	_, _, simple = t.walk(0, func(l Leaf) { leaves = append(leaves, l) })
	return leaves, simple
}

// walk lays an aggregate out once, reporting each scalar leaf at its offset from
// base and returning the aggregate's size and alignment. emit may be nil, so the
// callers that want only the size pay nothing for the ones that want the leaves.
//
// simple is false when a union or an opaque type was involved: their leaves
// overlap, or are unknown, so a caller reasoning about a flat sequence cannot
// use the answer.
func (t *AggType) walk(base int, emit func(Leaf)) (size, align int, simple bool) {
	if t.Opaque {
		align = t.Align
		if align == 0 {
			align = 1
		}
		return t.Size, align, false
	}
	simple = true
	if t.Union {
		simple = false
		align = 1
		for _, c := range t.Cases {
			s, a := walkFields(c, base, emit, t.Packed)
			if s > size {
				size = s
			}
			if a > align {
				align = a
			}
		}
	} else {
		size, align = walkFieldsSimple(t.Fields, base, emit, &simple, t.Packed)
	}
	if t.Align > align {
		align = t.Align // explicit alignment raises the minimum
	}
	return roundUpInt(size, align), align, simple
}

// walkFields lays out a sequence of struct fields, reporting each leaf at its
// offset and returning the size (before final rounding) and alignment.
func walkFields(fields []Field, base int, emit func(Leaf), packed bool) (size, align int) {
	simple := true
	return walkFieldsSimple(fields, base, emit, &simple, packed)
}

// walkFieldsSimple is walkFields, and clears simple when a nested type is one
// whose leaves cannot be read as a flat sequence. When packed, members follow one
// another with no inter-member padding and no field raises the aggregate's
// alignment (__attribute__((packed))).
func walkFieldsSimple(fields []Field, base int, emit func(Leaf), simple *bool, packed bool) (size, align int) {
	align = 1
	off := 0
	for _, f := range fields {
		fs, fa := f.sizeAlign()
		if !packed {
			if fa > align {
				align = fa
			}
			off = roundUpInt(off, fa)
		}
		for i := 0; i < f.count(); i++ {
			if f.Type != nil {
				if _, _, s := f.Type.walk(base+off, emit); !s {
					*simple = false
				}
			} else if emit != nil {
				emit(Leaf{Sub: f.Sub, Off: base + off})
			}
			off += fs
		}
	}
	return off, align
}

func roundUpInt(n, a int) int {
	if a <= 0 {
		return n
	}
	return ((n + a - 1) / a) * a
}
