package interp

import "github.com/evanphx/cg12/ir"

// vaState is the iteration state of one va_list: the variadic arguments the
// enclosing call passed, and how far va_arg has consumed them.
type vaState struct {
	args []Value
	next int
}

// A va_list is modelled as an 8-byte token stored at the address va_start was
// given; the token indexes mc.vaLists. Copying a va_list by copying its bytes
// therefore shares iteration state — a divergence from C's value-copy semantics
// that is documented and acceptable for the corpus.
func (mc *Machine) execVaStart(fr *frame, in *ir.Instr) error {
	av, err := mc.evalRef(fr, in.Arg(0))
	if err != nil {
		return err
	}
	token := uint64(len(mc.vaLists))
	mc.vaLists = append(mc.vaLists, vaState{args: fr.varargs})
	if !mc.mem.store(av.u64(), 8, token) {
		return mc.trapf("va_start: list storage at %#x unmapped", av.u64())
	}
	return nil
}

func (mc *Machine) execVaArg(fr *frame, in *ir.Instr) error {
	av, err := mc.evalRef(fr, in.Arg(0))
	if err != nil {
		return err
	}
	token, ok := mc.mem.load(av.u64(), 8)
	if !ok || token >= uint64(len(mc.vaLists)) {
		return mc.trapf("va_arg: invalid va_list at %#x", av.u64())
	}
	st := &mc.vaLists[token]
	v, ok := st.take()
	if !ok {
		return mc.trapf("va_arg: no more variadic arguments")
	}
	if !in.To.IsNone() {
		fr.vals[in.To.ID] = v.asClass(in.Cls)
	}
	return nil
}

func (st *vaState) take() (Value, bool) {
	if st.next >= len(st.args) {
		return Value{}, false
	}
	v := st.args[st.next]
	st.next++
	return v, true
}
