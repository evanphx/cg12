package ir

// This file holds the architecture-neutral half of Go-ABI support: the IR-to-IR
// annotation pass that discovers pointer-bearing stack words, the aggregate
// flattening algebra Go's ABIInternal is defined in terms of, and the small
// vocabulary helpers both backends need to read it.
//
// None of it names a register, an instruction encoding, or a frame layout. The
// architecture-dependent half -- which register a flattened part lands in, and
// how a byte offset inside an allocation becomes a frame-word index for the
// stack map -- stays in the backend, because those are the two places the
// machine actually shows through. On arm64 the frame record occupies [0,16) at
// the bottom of the frame and roots are positive offsets from x29 decoded as
// (offset-16)/8; on amd64 they are negative from rbp. That conversion is frame
// geometry and belongs to the backend that owns the frame.
//
// This lived in the arm64 backend alone, which is why amd64 has no Go-ABI
// support to speak of: every one of these routines would have had to be written
// a second time, with nothing keeping the two copies in step. The same reasoning
// moved HoistAllocas out of arm64 (see lower/alloca.go).

// PointerWordBytes is the granularity of a GC pointer word: the collector scans
// the stack a pointer at a time, so only offsets that are a multiple of this can
// name a root. It is the pointer size of every target with a managed frame
// (arm64 and amd64 are both LP64). A 32-bit managed target would have to thread
// a target-dependent value through the routines below instead.
const PointerWordBytes = 8

// InferStackPointerWords records, for every stack allocation in f, which of its
// words hold managed pointers, in f.StackPointerWords.
//
// It is a pure IR-to-IR pass: it reads only frontend IR -- the defining
// instruction of each temporary, the constant table, and each temporary's GCRef
// flag -- and writes only f.StackPointerWords. It depends on nothing a backend
// computes, so it can run once before lowering or emission, in any order
// relative to them.
//
// A store is recorded when its value is a managed pointer (GCRef, or the result
// of an allocation, which is an interior pointer into a frame the collector may
// move) and its address resolves to a constant offset from an allocation. The
// offsets are byte offsets from the start of the allocation; turning them into
// frame words is the backend's job, since only the backend knows where the
// allocation sits and which direction the frame grows.
func InferStackPointerWords(f *Func) {
	definitions := make(map[uint32]*Instr)
	for _, block := range f.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if instruction.To.Kind == RefTemp {
				definitions[instruction.To.ID] = instruction
			}
		}
	}

	// resolveAddress peels a chain of constant additions off an address to find
	// the allocation it points into, and the byte offset within it.
	var resolveAddress func(Ref) (uint32, int, bool)
	resolveAddress = func(reference Ref) (uint32, int, bool) {
		if reference.Kind != RefTemp {
			return 0, 0, false
		}
		instruction := definitions[reference.ID]
		if instruction == nil {
			return 0, 0, false
		}
		if instruction.Op.IsAlloc() {
			return reference.ID, 0, true
		}
		if instruction.Op != OAdd || len(instruction.Args) != 2 {
			return 0, 0, false
		}
		base, offset, ok := resolveAddress(instruction.Args[0])
		if !ok || instruction.Args[1].Kind != RefConst {
			return 0, 0, false
		}
		constant := f.Consts[instruction.Args[1].ID]
		if constant.Kind != ConstInt {
			return 0, 0, false
		}
		return base, offset + int(constant.Int), true
	}

	if f.StackPointerWords == nil {
		f.StackPointerWords = make(map[uint32]map[int]bool)
	}
	for _, block := range f.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if !instruction.Op.IsStore() || len(instruction.Args) < 2 {
				continue
			}
			value := instruction.Args[0]
			if value.Kind != RefTemp {
				continue
			}
			definition := definitions[value.ID]
			managed := f.Temp(value).GCRef || definition != nil && definition.Op.IsAlloc()
			if !managed {
				continue
			}
			allocation, offset, ok := resolveAddress(instruction.Args[1])
			if !ok || offset%PointerWordBytes != 0 {
				continue
			}
			if f.StackPointerWords[allocation] == nil {
				f.StackPointerWords[allocation] = make(map[int]bool)
			}
			f.StackPointerWords[allocation][offset] = true
		}
	}
}

// MarkAggregatePointerWords records the pointer fields of an aggregate held in
// the stack slot named by slot, so the collector scans them while the slot is
// live across a call.
//
// InferStackPointerWords finds pointers that IR stores put into a slot; this
// covers the complementary case, where a backend homes an aggregate into a slot
// and the pointer words are known from the type rather than from any store.
func (f *Func) MarkAggregatePointerWords(slot Ref, aggregate *AggType) {
	parts, ok := FlattenAggregate(aggregate)
	if !ok {
		return
	}
	if f.StackPointerWords == nil {
		f.StackPointerWords = make(map[uint32]map[int]bool)
	}
	for _, part := range parts {
		if !part.Pointer || part.Offset%PointerWordBytes != 0 {
			continue
		}
		if f.StackPointerWords[slot.ID] == nil {
			f.StackPointerWords[slot.ID] = make(map[int]bool)
		}
		f.StackPointerWords[slot.ID][part.Offset] = true
	}
}

// AllocAggregate reserves a stack slot for an aggregate, appending the
// allocation to out and returning the temporary holding its address.
//
// The allocation op is chosen from the aggregate's alignment alone, which is a
// property of the type and not of the machine.
func (f *Func) AllocAggregate(aggregate *AggType, out *[]Instr) Ref {
	size, alignment := aggregate.Layout()
	if size == 0 {
		size = 1
	}
	var operation Op
	switch {
	case alignment > 8:
		operation = OAlloc16
	case alignment > 4:
		operation = OAlloc8
	default:
		operation = OAlloc4
	}
	slot := f.NewTemp("", ClsL)
	*out = append(*out, Instr{Op: operation, Cls: ClsL, To: slot, Args: []Ref{f.Long(int64(size))}})
	// The aggregate is addressed through this temporary after nested calls. Under a
	// managed (copied) stack that interior pointer must be relocated and its
	// pointer words scanned, so the collector tracks it. A fixed C/Ruby stack does
	// not move, so the slot is an ordinary local needing no GC metadata.
	if f.UsesManagedFrame() {
		f.MarkGCRef(slot)
		f.MarkAggregatePointerWords(slot, aggregate)
	}
	return slot
}

// AggPart is one recursively flattened scalar of an aggregate: its memory width,
// its byte offset from the start of the aggregate, and whether it is a managed
// pointer. Go's ABIInternal is defined over exactly this sequence -- it assigns
// each part to its own integer or floating-point register -- so which register a
// part receives is the backend's to add alongside.
type AggPart struct {
	Sub     SubCls
	Offset  int
	Pointer bool
}

// FlattenAggregate returns the scalar parts of an aggregate in layout order, or
// ok == false when the aggregate has no register-assignable flat form: a union,
// an opaque type, or a struct containing a non-trivial inline array, none of
// which ABIInternal ever passes in registers.
//
// Two details of the placement rule differ from AggType.Leaves and are preserved
// deliberately, because the Go ABI has always behaved this way and changing
// either would change generated code: Packed is not honoured (fields are placed
// at their natural alignment regardless), and the opaque/union rejection applies
// only to the top-level aggregate -- a nested one is walked through Fields, which
// neither kind uses, so it contributes no parts and flattening still succeeds.
// See TestFlattenAggregateIgnoresPackedLayout and
// TestFlattenAggregateAcceptsNestedOpaqueAndUnionAsEmpty, which pin both.
func FlattenAggregate(aggregate *AggType) ([]AggPart, bool) {
	if aggregate == nil || aggregate.Opaque || aggregate.Union {
		return nil, false
	}

	var parts []AggPart
	var walk func(*AggType, int) bool
	walk = func(current *AggType, base int) bool {
		offset := 0
		for _, field := range current.Fields {
			size, alignment := field.SizeAlign()
			offset = roundUpInt(offset, alignment)
			count := field.Count
			if count <= 0 {
				count = 1
			}
			if count > 1 {
				// ABIInternal never register-assigns a non-trivial array, even
				// when all of its elements would otherwise fit.
				return false
			}
			if field.Type != nil {
				if !walk(field.Type, base+offset) {
					return false
				}
			} else {
				parts = append(parts, AggPart{
					Sub:     field.Sub,
					Offset:  base + offset,
					Pointer: field.Pointer,
				})
			}
			offset += size
		}
		return true
	}

	if !walk(aggregate, 0) {
		return nil, false
	}
	return parts, true
}

// AggregatePointerOffsets returns the byte offsets of every managed pointer
// field in an aggregate, including inside nested aggregates and inline arrays.
//
// Unlike FlattenAggregate this does not require a register-assignable shape: an
// aggregate passed in memory still needs its pointer words described to the
// collector. A union or opaque type contributes nothing, because which of its
// overlapping fields is live is not known from the type.
func AggregatePointerOffsets(aggregate *AggType) []int {
	var offsets []int
	var walk func(*AggType, int)
	walk = func(current *AggType, base int) {
		if current == nil || current.Opaque || current.Union {
			return
		}
		offset := 0
		for _, field := range current.Fields {
			size, alignment := field.SizeAlign()
			offset = roundUpInt(offset, alignment)
			count := field.Count
			if count <= 0 {
				count = 1
			}
			for index := 0; index < count; index++ {
				fieldOffset := base + offset + index*size
				if field.Type != nil {
					walk(field.Type, fieldOffset)
				} else if field.Pointer {
					offsets = append(offsets, fieldOffset)
				}
			}
			offset += size * count
		}
	}
	walk(aggregate, 0)
	return offsets
}

// ValueGroupAt returns the group covering the parameter or argument at index, if
// one does. Groups are few, so a scan beats a map.
func ValueGroupAt(groups []ValueGroup, index int) (ValueGroup, bool) {
	for _, group := range groups {
		if group.Index == index {
			return group, true
		}
	}
	return ValueGroup{}, false
}

// LoadOpForSub returns the load op that reads one aggregate field of the given
// sub-class, preserving its signedness. Every SubCls is covered, so the panic is
// reachable only from an invalid value.
func LoadOpForSub(sub SubCls) Op {
	switch sub {
	case SubB:
		return OLoadsb
	case SubUB:
		return OLoadub
	case SubH:
		return OLoadsh
	case SubUH:
		return OLoaduh
	case SubW:
		return OLoadsw
	case SubL:
		return OLoadl
	case SubS:
		return OLoads
	case SubD:
		return OLoadd
	case SubQ:
		return OLoadq
	default:
		panic("ir: unsupported aggregate field sub-class")
	}
}

// StoreOpForSub returns the store op that writes one aggregate field of the
// given sub-class. Stores carry no signedness, so the signed and unsigned
// sub-classes of a width share an op.
func StoreOpForSub(sub SubCls) Op {
	switch sub {
	case SubB, SubUB:
		return OStoreb
	case SubH, SubUH:
		return OStoreh
	case SubW:
		return OStorew
	case SubL:
		return OStorel
	case SubS:
		return OStores
	case SubD:
		return OStored
	case SubQ:
		return OStoreq
	default:
		panic("ir: unsupported aggregate field sub-class")
	}
}
