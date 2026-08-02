package opt

import "github.com/evanphx/cg12/ir"

type localSlot struct {
	base   locKey
	offset int64
}

// LowerHeapAllocations promotes typed heap-allocation candidates whose
// pointers provably remain local to stack slots. Candidates that may escape
// are lowered to ordinary allocator calls.
//
// Each decision is appended to module.AllocDecisions as it is made. That record
// is diagnostic only -- nothing in the compiler reads it -- but it is the only
// place the frame half of the decision survives: a promoted candidate becomes a
// bare OAlloc carrying a byte size and nothing else, so after this pass runs
// there is no way to ask the IR which frame slots used to be allocations, or of
// what type. See AllocationCensus.
func LowerHeapAllocations(module *ir.Module) bool {
	changed := false
	for _, function := range module.Funcs {
		if function.Start == nil {
			continue
		}
		if lowerFunctionHeapAllocations(function, &module.AllocDecisions) {
			changed = true
		}
	}
	return changed
}

func lowerFunctionHeapAllocations(function *ir.Func, decisions *[]ir.AllocDecision) bool {
	aliases := newAliasInfo(function)
	definitions := make(map[uint32]ir.Instr)
	bases := make(map[uint32]uint32)
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.To.Kind == ir.RefTemp {
				definitions[instruction.To.ID] = instruction
			}
			if instruction.Op == ir.OHeapAlloc && instruction.To.Kind == ir.RefTemp {
				bases[instruction.To.ID] = instruction.To.ID
			}
		}
	}
	if len(bases) == 0 {
		return false
	}

	escaped := make(map[uint32]bool)
	slotBases := make(map[localSlot]uint32)
	conflictedSlots := make(map[localSlot]bool)
	for updated := true; updated; {
		updated = false
		for id, instruction := range definitions {
			if _, known := bases[id]; known {
				continue
			}
			base, ok := derivedHeapBase(instruction, bases)
			if ok {
				bases[id] = base
				updated = true
			}
		}
		for _, block := range function.Blocks {
			for _, phi := range block.Phis {
				base, found, conflict := phiHeapBase(phi, bases)
				if conflict {
					for _, argument := range phi.Args {
						if argumentBase, tracked := heapBase(argument, bases); tracked {
							escaped[argumentBase] = true
						}
					}
					continue
				}
				if !found || phi.To.Kind != ir.RefTemp {
					continue
				}
				if previous, exists := bases[phi.To.ID]; exists {
					if previous != base {
						escaped[previous] = true
						escaped[base] = true
					}
					continue
				}
				bases[phi.To.ID] = base
				updated = true
			}
		}
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				destination, source, size, memoryCopy := memoryCopyOperands(function, instruction)
				if memoryCopy {
					sourceLocation := aliases.locOf(source, int(size))
					destinationLocation := aliases.locOf(destination, int(size))
					if sourceLocation.class == cLocal {
						for sourceSlot, base := range slotBases {
							if sourceSlot.base != sourceLocation.key() {
								continue
							}
							if sourceSlot.offset < sourceLocation.offset || sourceSlot.offset+8 > sourceLocation.offset+size {
								continue
							}
							if destinationLocation.class != cLocal {
								escaped[base] = true
								continue
							}
							destinationSlot := localSlot{
								base:   destinationLocation.key(),
								offset: destinationLocation.offset + sourceSlot.offset - sourceLocation.offset,
							}
							if conflictedSlots[destinationSlot] {
								escaped[base] = true
								continue
							}
							if previous, exists := slotBases[destinationSlot]; exists && previous != base {
								escaped[previous] = true
								escaped[base] = true
								delete(slotBases, destinationSlot)
								conflictedSlots[destinationSlot] = true
								continue
							}
							if _, exists := slotBases[destinationSlot]; !exists {
								slotBases[destinationSlot] = base
								updated = true
							}
						}
					}
				}
				if instruction.Op.IsStore() {
					base, tracked := heapBase(instruction.Arg(0), bases)
					location := aliases.locOf(instruction.Arg(1), 1)
					if !tracked || location.class != cLocal {
						continue
					}
					slot := localSlot{base: location.key(), offset: location.offset}
					if conflictedSlots[slot] {
						escaped[base] = true
						continue
					}
					if previous, exists := slotBases[slot]; exists && previous != base {
						escaped[previous] = true
						escaped[base] = true
						delete(slotBases, slot)
						conflictedSlots[slot] = true
						continue
					}
					if _, exists := slotBases[slot]; !exists {
						slotBases[slot] = base
						updated = true
					}
					continue
				}
				if !instruction.Op.IsLoad() || instruction.To.Kind != ir.RefTemp {
					continue
				}
				location := aliases.locOf(instruction.Arg(0), 1)
				if location.class != cLocal {
					continue
				}
				slot := localSlot{base: location.key(), offset: location.offset}
				base, tracked := slotBases[slot]
				if !tracked {
					continue
				}
				if previous, exists := bases[instruction.To.ID]; exists && previous != base {
					escaped[previous] = true
					escaped[base] = true
					continue
				}
				if _, exists := bases[instruction.To.ID]; !exists {
					bases[instruction.To.ID] = base
					updated = true
				}
			}
		}
	}

	mark := func(reference ir.Ref) {
		if reference.Kind == ir.RefTemp {
			if base, ok := bases[reference.ID]; ok {
				escaped[base] = true
			}
		}
	}
	for _, block := range function.Blocks {
		for _, phi := range block.Phis {
			if _, tracked := heapBase(phi.To, bases); tracked {
				continue
			}
			for _, argument := range phi.Args {
				mark(argument)
			}
		}
		for _, instruction := range block.Instrs {
			switch {
			case instruction.Op == ir.OHeapAlloc:
				// The allocator, type descriptor, and size are not uses of the result.
			case instruction.Op.IsLoad():
				// Reading through the candidate keeps it local.
			case instruction.Op == ir.OCmp:
				// Comparing a pointer observes its value but cannot retain it.
			case instruction.Op.IsStore():
				// Frontend variables live in ordinary stack allocations. Saving a
				// candidate in a non-escaping local slot does not make the pointed-to
				// object escape; storing it anywhere externally reachable does.
				if aliases.locOf(instruction.Arg(1), 1).class != cLocal {
					mark(instruction.Arg(0))
				}
			case isTrackedHeapDerivation(instruction, bases):
				// Copies, casts, and constant pointer offsets preserve locality.
			case isAtomicPointerStore(function, instruction):
				// The destination is observed only for the duration of the store.
				// This is the write-barrier form emitted for pointer fields before
				// escape lowering knows whether the enclosing object is stack-local.
				destination := instruction.Arg(1)
				if _, localCandidate := heapBase(destination, bases); localCandidate {
					mark(instruction.Arg(2))
				} else if aliases.locOf(destination, 1).class != cLocal {
					mark(instruction.Arg(2))
				}
			case benignMemoryCall(function, instruction):
				// memcpy/memset observe the storage but do not retain its address.
			default:
				for _, argument := range instruction.Args {
					mark(argument)
				}
			}
		}
		mark(block.Jmp.Arg)
		for _, argument := range block.Jmp.Args {
			mark(argument)
		}
	}

	for _, block := range function.Blocks {
		lowered := make([]ir.Instr, 0, len(block.Instrs))
		for _, original := range block.Instrs {
			instruction := original
			if instruction.Op != ir.OHeapAlloc || instruction.To.Kind != ir.RefTemp {
				lowered = append(lowered, instruction)
				continue
			}
			if escaped[instruction.To.ID] {
				recordAllocDecision(decisions, function, original, ir.AllocOnHeap)
				instruction.Op = ir.OCall
				instruction.Args = instruction.Args[:2]
				instruction.Aux = 0
				lowered = append(lowered, instruction)
				continue
			}
			recordAllocDecision(decisions, function, original, ir.AllocInFrame)

			size := instruction.Args[2]
			switch instruction.Aux {
			case 4:
				instruction.Op = ir.OAlloc4
			case 8:
				instruction.Op = ir.OAlloc8
			case 16:
				instruction.Op = ir.OAlloc16
			default:
				panic("opt: invalid heap allocation alignment")
			}
			instruction.Args = []ir.Ref{size}
			instruction.Aux = 0
			lowered = append(lowered, instruction)
			lowered = append(lowered, ir.Instr{
				Op:   ir.OCall,
				Cls:  ir.ClsW,
				Args: []ir.Ref{function.Sym("goc_memset", 0), instruction.To, function.Word(0), size},
				Pos:  instruction.Pos,
			})
		}
		block.Instrs = lowered
	}
	return true
}

// recordAllocDecision notes where one heap-allocation candidate landed.
// candidate must still be the unrewritten OHeapAlloc, whose first two arguments
// are the allocator and the type descriptor.
func recordAllocDecision(decisions *[]ir.AllocDecision, function *ir.Func, candidate ir.Instr, placement ir.AllocPlacement) {
	if decisions == nil {
		return
	}
	*decisions = append(*decisions, ir.AllocDecision{
		Func:      function.Name,
		Pos:       candidate.Pos,
		Allocator: constSymbolName(function, candidate.Args[0]),
		Type:      constSymbolName(function, candidate.Args[1]),
		Placement: placement,
	})
}

// constSymbolName returns the symbol name a reference names, or "" if it is not
// a symbol constant.
func constSymbolName(function *ir.Func, reference ir.Ref) string {
	if reference.Kind != ir.RefConst || int(reference.ID) >= len(function.Consts) {
		return ""
	}
	constant := function.Consts[reference.ID]
	if constant.Kind != ir.ConstSym {
		return ""
	}
	return constant.Sym
}

func phiHeapBase(phi *ir.Phi, bases map[uint32]uint32) (uint32, bool, bool) {
	var base uint32
	found := false
	for _, argument := range phi.Args {
		argumentBase, tracked := heapBase(argument, bases)
		if !tracked {
			continue
		}
		if found && argumentBase != base {
			return 0, false, true
		}
		base = argumentBase
		found = true
	}
	return base, found, false
}

func isAtomicPointerStore(function *ir.Func, instruction ir.Instr) bool {
	if instruction.Op != ir.OCall || len(instruction.Args) != 3 {
		return false
	}
	callee := instruction.Arg(0)
	if callee.Kind != ir.RefConst {
		return false
	}
	constant := function.Consts[callee.ID]
	if constant.Kind != ir.ConstSym {
		return false
	}
	return function.Module().SymAttrOf(constant.Sym).Has(ir.SymAtomicPointerStore)
}

func derivedHeapBase(instruction ir.Instr, bases map[uint32]uint32) (uint32, bool) {
	if instruction.Op == ir.OCopy || instruction.Op == ir.OCast {
		return heapBase(instruction.Arg(0), bases)
	}
	if (instruction.Op != ir.OAdd && instruction.Op != ir.OSub) || instruction.Cls != ir.ClsP {
		return 0, false
	}
	if base, ok := heapBase(instruction.Arg(0), bases); ok {
		return base, true
	}
	if instruction.Op == ir.OAdd {
		if base, ok := heapBase(instruction.Arg(1), bases); ok {
			return base, true
		}
	}
	return 0, false
}

func heapBase(reference ir.Ref, bases map[uint32]uint32) (uint32, bool) {
	if reference.Kind != ir.RefTemp {
		return 0, false
	}
	base, ok := bases[reference.ID]
	return base, ok
}

func isTrackedHeapDerivation(instruction ir.Instr, bases map[uint32]uint32) bool {
	if instruction.To.Kind != ir.RefTemp {
		return false
	}
	_, ok := bases[instruction.To.ID]
	return ok && instruction.Op != ir.OHeapAlloc
}

func benignMemoryCall(function *ir.Func, instruction ir.Instr) bool {
	if instruction.Op != ir.OCall || len(instruction.Args) == 0 {
		return false
	}
	callee := instruction.Arg(0)
	if callee.Kind != ir.RefConst {
		return false
	}
	constant := function.Consts[callee.ID]
	if constant.Kind != ir.ConstSym {
		return false
	}
	switch constant.Sym {
	case "memset", "memcpy", "memmove", "memcmp",
		"goc_memset", "goc_memcpy", "goc_memmove", "goc_memcmp",
		"runtime.growslice":
		return true
	default:
		return false
	}
}

func memoryCopyOperands(function *ir.Func, instruction ir.Instr) (ir.Ref, ir.Ref, int64, bool) {
	if instruction.Op != ir.OCall || len(instruction.Args) != 4 {
		return ir.R, ir.R, 0, false
	}
	callee := instruction.Arg(0)
	if callee.Kind != ir.RefConst {
		return ir.R, ir.R, 0, false
	}
	constant := function.Consts[callee.ID]
	if constant.Kind != ir.ConstSym {
		return ir.R, ir.R, 0, false
	}
	switch constant.Sym {
	case "memcpy", "memmove", "goc_memcpy", "goc_memmove":
	default:
		return ir.R, ir.R, 0, false
	}
	size, knownSize := constInt(function, instruction.Arg(3))
	if !knownSize || size < 0 {
		return ir.R, ir.R, 0, false
	}
	return instruction.Arg(1), instruction.Arg(2), size, true
}
