package opt

import "github.com/evanphx/cg12/ir"

type localSlot struct {
	base   string
	offset int64
}

// LowerHeapAllocations promotes typed heap-allocation candidates whose
// pointers provably remain local to stack slots. Candidates that may escape
// are lowered to ordinary allocator calls.
func LowerHeapAllocations(module *ir.Module) bool {
	changed := false
	for _, function := range module.Funcs {
		if function.Start != nil && lowerFunctionHeapAllocations(function) {
			changed = true
		}
	}
	return changed
}

func lowerFunctionHeapAllocations(function *ir.Func) bool {
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
			base, ok := derivedHeapBase(function, instruction, bases)
			if ok {
				bases[id] = base
				updated = true
			}
		}
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				if instruction.Op.IsStore() {
					base, tracked := heapBase(instruction.Arg(0), bases)
					location := aliases.locOf(instruction.Arg(1), 1)
					if !tracked || location.class != cLocal {
						continue
					}
					slot := localSlot{base: location.key, offset: location.offset}
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
				slot := localSlot{base: location.key, offset: location.offset}
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
			case instruction.Op.IsStore():
				// Frontend variables live in ordinary stack allocations. Saving a
				// candidate in a non-escaping local slot does not make the pointed-to
				// object escape; storing it anywhere externally reachable does.
				if aliases.locOf(instruction.Arg(1), 1).class != cLocal {
					mark(instruction.Arg(0))
				}
			case isTrackedHeapDerivation(instruction, bases):
				// Copies, casts, and constant pointer offsets preserve locality.
			case benignMemoryCall(function, instruction):
				// memcpy/memset observe the storage but do not retain its address.
			default:
				for _, argument := range instruction.Args {
					mark(argument)
				}
			}
		}
		mark(block.Jmp.Arg)
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
				instruction.Op = ir.OCall
				instruction.Args = instruction.Args[:2]
				instruction.Aux = 0
				lowered = append(lowered, instruction)
				continue
			}

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

func derivedHeapBase(function *ir.Func, instruction ir.Instr, bases map[uint32]uint32) (uint32, bool) {
	if instruction.Op == ir.OCopy || instruction.Op == ir.OCast {
		return heapBase(instruction.Arg(0), bases)
	}
	if instruction.Op != ir.OAdd && instruction.Op != ir.OSub {
		return 0, false
	}
	if base, ok := heapBase(instruction.Arg(0), bases); ok {
		if _, constant := constInt(function, instruction.Arg(1)); constant {
			return base, true
		}
	}
	if instruction.Op == ir.OAdd {
		if base, ok := heapBase(instruction.Arg(1), bases); ok {
			if _, constant := constInt(function, instruction.Arg(0)); constant {
				return base, true
			}
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
		"goc_memset", "goc_memcpy", "goc_memmove", "goc_memcmp":
		return true
	default:
		return false
	}
}
