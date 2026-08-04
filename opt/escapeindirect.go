package opt

import "github.com/evanphx/cg12/ir"

// An aggregate frontend variable is one level indirect: goc gives it a slot
// holding the *address* of stable backing storage, and reads that slot back
// every time the variable is named. `source := [4]*T{...}` is
//
//	%slot    = alloc8 8            // the variable
//	%backing = alloc8 32           // its storage
//	store %backing, %slot
//	...
//	%p = load %slot                // every later use of `source`
//	goc_storep(%p, %box)           // source[0] = box
//
// aliasInfo resolves %backing to a stack allocation and %p to nothing at all: a
// load has no alloc base, so locOf answers cUnknown for it. The escape mark loop
// reads that as "not local storage" and calls the store a publication, which
// sends every pointer written into a local array or struct to the heap.
// `boxes := [4]*T{{1},{2},{3},{4}}` costs four allocations under goc and none
// under gc for exactly this reason.
//
// This file undoes goc's own indirection, for the escape analysis only.
//
// # Why not fix it in aliasInfo
//
// Because aliasInfo's cLocal means more than "a stack allocation". distinct()
// reads it as "unreachable through any other base" and answers "no alias" for
// cLocal against anything else -- which is what lets DSE and GVN move stores. A
// backing allocation whose address is in a slot *is* reachable through another
// base: through the pointer loaded back out of that slot. Reclassifying it there
// would tell those passes two aliasing accesses are distinct.
//
// So the resolution lives here, is used by analyzeCandidateEscapes and nothing
// else, and answers the one question the escape analysis asks: is this
// destination storage that cannot outlive the frame.

// indirectStorage resolves the pointers a function loads out of its own variable
// slots back to the frame storage they address.
type indirectStorage struct {
	aliases *aliasInfo
	// loaded maps a temp defined by a load out of an indirection slot to the
	// backing allocation that load produced the address of.
	loaded map[uint32]uint32
	// confined is the set of backing allocations reached that way whose address
	// provably stays inside the frame. It is a subset of loaded's values, and it
	// is what makes a store through one of them not a publication.
	confined map[uint32]bool
}

// resolveIndirectStorage builds the resolution for one function. It returns nil
// when the function has no indirection slot worth resolving, which is the common
// case for a function with no aggregate locals.
func resolveIndirectStorage(function *ir.Func, aliases *aliasInfo) *indirectStorage {
	slots := indirectionSlots(function, aliases)
	if len(slots) == 0 {
		return nil
	}
	confined := make(map[uint32]bool, len(slots))
	for _, backing := range slots {
		confined[backing] = true
	}
	dropUnconfinedBacking(function, aliases, slots, confined)
	loaded := loadsThroughSlots(function, aliases, slots, confined)
	if len(loaded) == 0 {
		return nil
	}
	return &indirectStorage{aliases: aliases, loaded: loaded, confined: confined}
}

// indirectionSlots finds the slots that hold exactly one backing address for the
// whole life of the function, and names that backing allocation.
//
// "Exactly one" is what makes the answer a fact rather than a guess: goc writes
// a variable's storage pointer once, where the variable is declared, and never
// again. A slot written twice -- or written by a block copy, which this cannot
// read -- is not one of these and is left alone.
func indirectionSlots(function *ir.Func, aliases *aliasInfo) map[uint32]uint32 {
	stored := make(map[uint32]uint32)
	rejected := make(map[uint32]bool)
	note := func(destination, value ir.Ref) {
		location := aliases.locOf(destination, 8)
		if location.keyKind != keyLocal || location.offset != 0 {
			// keyLocal rather than "any stack allocation": the slot's own address
			// must not escape, or something outside could write a different
			// backing pointer into it.
			return
		}
		slot := uint32(location.keyID)
		if rejected[slot] {
			return
		}
		backing, isAlloc := backingAllocation(aliases, value)
		if !isAlloc {
			rejected[slot] = true
			delete(stored, slot)
			return
		}
		if previous, seen := stored[slot]; seen && previous != backing {
			rejected[slot] = true
			delete(stored, slot)
			return
		}
		stored[slot] = backing
	}
	writes := make(map[uint32]int)
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			value, destination, isStore := trackedPointerStore(function, instruction)
			if isStore {
				if location := aliases.locOf(destination, 8); location.keyKind == keyLocal {
					writes[uint32(location.keyID)]++
				}
				note(destination, value)
				continue
			}
			// A block copy into a slot writes it without being a pointer store,
			// and this cannot tell what it wrote.
			if destination, _, _, isCopy := memoryCopyOperands(function, instruction); isCopy {
				if location := aliases.locOf(destination, 8); location.keyKind == keyLocal {
					rejected[uint32(location.keyID)] = true
					delete(stored, uint32(location.keyID))
				}
			}
		}
	}
	for slot := range stored {
		if writes[slot] != 1 || rejected[slot] {
			delete(stored, slot)
		}
	}
	return stored
}

// backingAllocation names the allocation a stored pointer is exactly the address
// of. A pointer into the middle of an allocation is not one: the slot would then
// address storage this cannot key consistently against a direct reference.
func backingAllocation(aliases *aliasInfo, value ir.Ref) (uint32, bool) {
	if value.Kind != ir.RefTemp {
		return 0, false
	}
	base, ok := aliases.allocBase(value.ID)
	if !ok {
		return 0, false
	}
	location := aliases.locOf(value, 8)
	if location.keyKind != keyLocal && location.keyKind != keyEscaped {
		return 0, false
	}
	if location.offset != 0 {
		return 0, false
	}
	return base, true
}

// dropUnconfinedBacking removes from confined every backing allocation with a
// use that can carry its address, or its contents, out of the frame.
//
// The allowed uses are the ones that keep the storage where it is: reading and
// writing through it, the write barrier's destination, a memory helper, a
// lifetime marker, a constant-offset derivation, and the one store that put its
// address in its own slot. Anything else -- a call argument, a return, a phi, a
// store anywhere but that slot -- means something outside this function may be
// able to reach what the storage holds, and a pointer written into it is a
// publication after all.
func dropUnconfinedBacking(function *ir.Func, aliases *aliasInfo, slots map[uint32]uint32, confined map[uint32]bool) {
	backingSlot := make(map[uint32]uint32, len(slots))
	for slot, backing := range slots {
		backingSlot[backing] = slot
	}
	drop := func(reference ir.Ref) {
		if reference.Kind != ir.RefTemp {
			return
		}
		if base, ok := aliases.allocBase(reference.ID); ok {
			delete(confined, base)
		}
	}
	storesItsOwnAddress := func(instruction ir.Instr, value, destination ir.Ref) bool {
		if value.Kind != ir.RefTemp {
			return false
		}
		base, ok := aliases.allocBase(value.ID)
		if !ok {
			return false
		}
		slot, indirect := backingSlot[base]
		if !indirect {
			return false
		}
		location := aliases.locOf(destination, 8)
		return location.keyKind == keyLocal && uint32(location.keyID) == slot && location.offset == 0
	}
	for _, block := range function.Blocks {
		for _, phi := range block.Phis {
			for _, argument := range phi.Args {
				drop(argument)
			}
		}
		for _, instruction := range block.Instrs {
			switch {
			case instruction.Op.IsLoad():
				// The address operand is a read through the storage.
			case instruction.Op.IsAlloc(), instruction.Op.IsLifetime():
			case instruction.Op.IsStore():
				if !storesItsOwnAddress(instruction, instruction.Arg(0), instruction.Arg(1)) {
					drop(instruction.Arg(0))
				}
			case isAtomicPointerStore(function, instruction):
				if !storesItsOwnAddress(instruction, instruction.Arg(2), instruction.Arg(1)) {
					drop(instruction.Arg(2))
				}
			case benignMemoryCall(function, instruction):
				// memset, memcmp and a block copy observe the storage without
				// carrying its address anywhere. growslice is in the same list
				// and does take a pointer it may keep, so it is refused.
				if constSymbolName(function, instruction.Arg(0)) == "runtime.growslice" {
					for _, argument := range instruction.Args[1:] {
						drop(argument)
					}
				}
			case (instruction.Op == ir.OAdd || instruction.Op == ir.OSub) && aliases.tracked(instruction.To):
				// A constant-offset derivation stays inside the allocation, and
				// the derived pointer's own uses are visited in their own right.
			default:
				for _, argument := range instruction.Args {
					drop(argument)
				}
			}
		}
		drop(block.Jmp.Arg)
		for _, argument := range block.Jmp.Args {
			drop(argument)
		}
	}
}

// loadsThroughSlots names every temp that is a load of a confined backing
// address out of its slot.
func loadsThroughSlots(function *ir.Func, aliases *aliasInfo, slots map[uint32]uint32, confined map[uint32]bool) map[uint32]uint32 {
	var loaded map[uint32]uint32
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if !instruction.Op.IsLoad() || instruction.To.Kind != ir.RefTemp {
				continue
			}
			location := aliases.locOf(instruction.Arg(0), 8)
			if location.keyKind != keyLocal || location.offset != 0 {
				continue
			}
			backing, indirect := slots[uint32(location.keyID)]
			if !indirect || !confined[backing] {
				continue
			}
			if loaded == nil {
				loaded = make(map[uint32]uint32, 1)
			}
			loaded[instruction.To.ID] = backing
		}
	}
	return loaded
}

// locOf is aliasInfo.locOf with the indirection resolved: a pointer loaded out
// of a variable's slot answers with the backing allocation's own key and class,
// so that a store through it and a store through the allocation directly are the
// same location to everything in analyzeCandidateEscapes.
func (indirect *indirectStorage) locOf(aliases *aliasInfo, reference ir.Ref, width int) locInfo {
	location := aliases.locOf(reference, width)
	if indirect == nil {
		return location
	}
	if location.keyKind == keyTemp {
		if backing, resolved := indirect.loaded[uint32(location.keyID)]; resolved {
			location.keyKind = keyLocal
			location.keyID = uint64(backing)
			location.keySym = ""
			location.class = cLocal
			return location
		}
		return location
	}
	// A direct reference to confined backing storage. aliasInfo calls it
	// cEscaped, because its address was stored into its own slot; here that
	// store is the indirection itself and says nothing about the frame.
	if location.keyKind == keyEscaped && indirect.confined[uint32(location.keyID)] {
		location.keyKind = keyLocal
		location.class = cLocal
	}
	return location
}

// escapeLocOf is the accessor analyzeCandidateEscapes uses, so that a nil
// resolution costs one branch.
func escapeLocOf(aliases *aliasInfo, indirect *indirectStorage, reference ir.Ref, width int) locInfo {
	if indirect == nil {
		return aliases.locOf(reference, width)
	}
	return indirect.locOf(aliases, reference, width)
}
