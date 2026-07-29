package amd64

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
)

// These tests cover AMD64_PARITY_PLAN B1's lowering half: which physical
// register each parameter, argument and result lands in once emissionConvention
// answers with the function's own convention.
//
// They are white-box on purpose. The execution tests in goabi_exec_test.go prove
// that a cg12-compiled ABIInternal call works end to end, but both sides of such
// a call are compiled by this backend, so execution alone cannot distinguish
// "ABIInternal" from "any self-consistent register assignment" -- which is
// exactly the state amd64 was in before this change. Naming the registers here is
// what pins the assignment to Go's ABI rather than to an internally coherent
// invention. Every constant below is Go's, not cg12's: IntArgRegs = RAX, RBX,
// RCX, RDI, RSI, R8, R9, R10, R11 (stdlib/src/internal/abi/abi_amd64.go).

// parameterRegisters lowers f and returns the physical register each OPar reads
// from, in parameter order, using -1 for a parameter that arrived on the stack.
func parameterRegisters(t *testing.T, m *ir.Module, f *ir.Func) []int {
	t.Helper()
	if err := lower(f, newCalleeConventions(m)); err != nil {
		t.Fatalf("lower(%s): %v", f.Name, err)
	}
	var regs []int
	for i := range f.Start.Instrs {
		in := &f.Start.Instrs[i]
		if in.Op != ir.OPar {
			break
		}
		if len(in.Args) == 0 {
			regs = append(regs, -1) // a stacked parameter carries its offset in Aux
			continue
		}
		regs = append(regs, f.Temp(in.Args[0]).Reg)
	}
	return regs
}

// argumentRegisters lowers f and returns the physical register each OArg of the
// first call writes to, in argument order, using -1 for a stacked argument.
func argumentRegisters(t *testing.T, m *ir.Module, f *ir.Func) []int {
	t.Helper()
	if err := lower(f, newCalleeConventions(m)); err != nil {
		t.Fatalf("lower(%s): %v", f.Name, err)
	}
	var regs []int
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op != ir.OArg {
				continue
			}
			if in.To.IsNone() {
				regs = append(regs, -1)
				continue
			}
			regs = append(regs, f.Temp(in.To).Reg)
		}
		if len(regs) > 0 {
			return regs
		}
	}
	return regs
}

// resultRegister lowers f and returns the physical register its return value is
// copied into.
func resultRegister(t *testing.T, m *ir.Module, f *ir.Func) int {
	t.Helper()
	if err := lower(f, newCalleeConventions(m)); err != nil {
		t.Fatalf("lower(%s): %v", f.Name, err)
	}
	for _, b := range f.Blocks {
		if b.Jmp.Kind != ir.JmpRet || b.Jmp.Arg.IsNone() {
			continue
		}
		return f.Temp(b.Jmp.Arg).Reg
	}
	t.Fatalf("function %s has no returned value", f.Name)
	return -1
}

func regNames(regs []int) []Reg {
	out := make([]Reg, len(regs))
	for i, r := range regs {
		out[i] = Reg(r)
	}
	return out
}

func requireRegs(t *testing.T, what string, got []int, want []Reg) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d registers %v, want %d %v", what, len(got), regNames(got), len(want), want)
	}
	for i := range want {
		if Reg(got[i]) != want[i] {
			t.Errorf("%s[%d] = %v, want %v (whole sequence %v, want %v)",
				what, i, Reg(got[i]), want[i], regNames(got), want)
		}
	}
}

// scalarFunc builds a function taking n long parameters and returning their sum.
func scalarFunc(m *ir.Module, name string, cc ir.CallConvention, n int) *ir.Func {
	f := m.NewFunc(name, ir.ClsL)
	f.CallConv = cc
	var ps []ir.Ref
	for i := 0; i < n; i++ {
		ps = append(ps, f.Param("p", ir.ClsL))
	}
	e := f.Entry()
	acc := f.Long(0)
	for _, p := range ps {
		acc = e.Add(ir.ClsL, acc, p)
	}
	e.Ret(acc)
	return f
}

// TestGoInternalParametersUseGoArgumentRegisters is the parameter row, and the
// one with no overlap at all to hide behind: System V starts at RDI, Go's
// ABIInternal at RAX, and the two sequences never coincide at any index.
func TestGoInternalParametersUseGoArgumentRegisters(t *testing.T) {
	m := ir.NewModule()
	f := scalarFunc(m, "closure", ir.CallConvGoInternal, 9)
	requireRegs(t, "ABIInternal parameters", parameterRegisters(t, m, f),
		[]Reg{RAX, RBX, RCX, RDI, RSI, R8, R9, R10, R11})

	m2 := ir.NewModule()
	g := scalarFunc(m2, "plain", ir.CallConvPlatform, 6)
	requireRegs(t, "System V parameters", parameterRegisters(t, m2, g),
		[]Reg{RDI, RSI, RDX, RCX, R8, R9})
}

// TestGoInternalStackParametersArePackedByNaturalSize covers the second half of
// the parameter rule. Once the nine integer registers are spent, ABIInternal
// packs the remainder at natural alignment (a 4-byte value takes four bytes),
// where System V gives every scalar a whole eight-byte slot.
func TestGoInternalStackParametersArePackedByNaturalSize(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("closure", ir.ClsW)
	f.CallConv = ir.CallConvGoInternal
	for i := 0; i < 9; i++ {
		f.Param("p", ir.ClsL)
	}
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, a, b))

	if err := lower(f, newCalleeConventions(m)); err != nil {
		t.Fatalf("lower: %v", err)
	}
	var offsets []int64
	for i := range f.Start.Instrs {
		in := &f.Start.Instrs[i]
		if in.Op != ir.OPar {
			break
		}
		if len(in.Args) == 0 {
			offsets = append(offsets, in.Aux)
		}
	}
	want := []int64{0, 4}
	if len(offsets) != len(want) {
		t.Fatalf("got %d stacked parameters at %v, want %d at %v", len(offsets), offsets, len(want), want)
	}
	for i := range want {
		if offsets[i] != want[i] {
			t.Errorf("stacked ABIInternal parameter %d at byte %d, want %d (natural-size packing)",
				i, offsets[i], want[i])
		}
	}
}

// TestCallArgumentsFollowTheCalleeNotTheCaller is the hazard §3.3(d) of the plan
// exists for. Both directions are checked, because both occur in goc output: an
// ABIInternal closure makes ordinary unmarked calls to platform-ABI functions,
// and platform-ABI functions call closures. Resolving either from the enclosing
// function -- arm64's rule -- puts every argument in the wrong register here,
// since the two sequences share nothing.
func TestCallArgumentsFollowTheCalleeNotTheCaller(t *testing.T) {
	t.Run("platform caller, ABIInternal callee", func(t *testing.T) {
		m := ir.NewModule()
		scalarFunc(m, "closure", ir.CallConvGoInternal, 3)
		caller := m.NewFunc("caller", ir.ClsL)
		e := caller.Entry()
		e.Ret(e.CallWithConvention(ir.ClsL, ir.CallConvGoInternal, caller.Sym("closure", 0),
			caller.Long(1), caller.Long(2), caller.Long(3)))
		requireRegs(t, "arguments to a closure", argumentRegisters(t, m, caller), []Reg{RAX, RBX, RCX})
	})

	t.Run("ABIInternal caller, platform callee", func(t *testing.T) {
		m := ir.NewModule()
		scalarFunc(m, "plain", ir.CallConvPlatform, 3)
		caller := m.NewFunc("caller", ir.ClsL)
		caller.CallConv = ir.CallConvGoInternal
		e := caller.Entry()
		e.Ret(e.Call(ir.ClsL, caller.Sym("plain", 0), caller.Long(1), caller.Long(2), caller.Long(3)))
		requireRegs(t, "unmarked call out of a closure", argumentRegisters(t, m, caller), []Reg{RDI, RSI, RDX})
	})

	t.Run("call to a symbol outside the module", func(t *testing.T) {
		// ABIInternal exists only inside a module cg12 compiled, so an unresolvable
		// callee is the platform ABI by definition -- even from a closure.
		m := ir.NewModule()
		caller := m.NewFunc("caller", ir.ClsL)
		caller.CallConv = ir.CallConvGoInternal
		e := caller.Entry()
		e.Ret(e.Call(ir.ClsL, caller.Sym("memcpy", 0), caller.Long(1), caller.Long(2)))
		requireRegs(t, "call to an external symbol", argumentRegisters(t, m, caller), []Reg{RDI, RSI})
	})
}

// TestGoInternalResultsComeBackInTheArgumentRegisters is the result row. A
// single scalar is the case where the two conventions agree by coincidence (RAX,
// XMM0), so it is checked for both to pin that the coincidence is what is
// happening rather than the platform table leaking through; the aggregate case
// below is where they part company.
func TestGoInternalResultsComeBackInTheArgumentRegisters(t *testing.T) {
	for _, tc := range []struct {
		name string
		cc   ir.CallConvention
		cls  ir.Cls
		want Reg
	}{
		{"ABIInternal integer", ir.CallConvGoInternal, ir.ClsL, RAX},
		{"System V integer", ir.CallConvPlatform, ir.ClsL, RAX},
		{"ABIInternal float", ir.CallConvGoInternal, ir.ClsD, XMM(0)},
		{"System V float", ir.CallConvPlatform, ir.ClsD, XMM(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := ir.NewModule()
			f := m.NewFunc("f", tc.cls)
			f.CallConv = tc.cc
			p := f.Param("p", tc.cls)
			e := f.Entry()
			e.Ret(e.Add(tc.cls, p, p))
			if got := resultRegister(t, m, f); Reg(got) != tc.want {
				t.Errorf("result register = %v, want %v", Reg(got), tc.want)
			}
		})
	}

	// The second result register is where the two conventions diverge: System V
	// returns a two-eightbyte aggregate in RAX/RDX, ABIInternal in the argument
	// registers RAX/RBX.
	if got, want := conventionABI(ir.CallConvPlatform).retIntRegs[1], RDX; got != want {
		t.Errorf("System V second result register = %v, want %v", got, want)
	}
	if got, want := conventionABI(ir.CallConvGoInternal).retIntRegs[1], RBX; got != want {
		t.Errorf("ABIInternal second result register = %v, want %v", got, want)
	}
}

// TestGoInternalAggregatesAreDecomposedByField pins the rule that separates
// ABIInternal from System V for aggregates: Go gives every flattened *field* its
// own register, where System V classifies the value into eightbytes and packs two
// 32-bit fields into one. The same struct therefore occupies two registers under
// one convention and one under the other, which is why classifyAgg/assignAgg must
// never be used for the Go path.
func TestGoInternalAggregatesAreDecomposedByField(t *testing.T) {
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}

	m := ir.NewModule()
	m.AddType(pair)
	f := m.NewFunc("closure", ir.ClsW)
	f.CallConv = ir.CallConvGoInternal
	p := f.NewTemp("p", ir.ClsL)
	f.Temp(p).Agg = pair
	f.Params = append(f.Params, f.Temp(p))
	tail := f.Param("tail", ir.ClsL)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, e.Load(ir.ClsW, p), e.Extsw(ir.ClsW, tail)))

	if err := lower(f, newCalleeConventions(m)); err != nil {
		t.Fatalf("lower: %v", err)
	}
	// Two fields take RAX and RBX, so the scalar that follows them takes RCX. A
	// System V eightbyte classification would have packed both fields into RDI and
	// left the scalar in RSI.
	var found []Reg
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op == ir.OPar && len(in.Args) == 1 {
				found = append(found, Reg(f.Temp(in.Args[0]).Reg))
			}
			if in.Op == ir.OStorew && len(in.Args) == 2 && in.Args[0].Kind == ir.RefTemp && f.Temp(in.Args[0]).Fixed {
				found = append(found, Reg(f.Temp(in.Args[0]).Reg))
			}
		}
	}
	want := []Reg{RCX, RAX, RBX} // the scalar's OPar, then the two field stores
	requireRegsSet(t, "ABIInternal aggregate fields plus the following scalar", found, want)
}

func requireRegsSet(t *testing.T, what string, got, want []Reg) {
	t.Helper()
	seen := map[Reg]bool{}
	for _, r := range got {
		seen[r] = true
	}
	for _, r := range want {
		if !seen[r] {
			t.Errorf("%s: %v is not among the registers used (%v, want %v)", what, r, got, want)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s: got %d registers %v, want %d %v", what, len(got), got, len(want), want)
	}
}

// TestGoInternalCallCarriesItsConventionToTheEmitter checks the record lowering
// leaves behind. The emitter has to know, per call, whether it may write the
// System V vararg count into RAX -- ABIInternal's argument register 0 -- and the
// only thing that can tell it is the convention lowerCalls stamped on the
// instruction.
func TestGoInternalCallCarriesItsConventionToTheEmitter(t *testing.T) {
	m := ir.NewModule()
	scalarFunc(m, "closure", ir.CallConvGoInternal, 1)
	scalarFunc(m, "plain", ir.CallConvPlatform, 1)
	caller := m.NewFunc("caller", ir.ClsL)
	e := caller.Entry()
	e.CallWithConvention(ir.ClsL, ir.CallConvGoInternal, caller.Sym("closure", 0), caller.Long(1))
	e.Ret(e.Call(ir.ClsL, caller.Sym("plain", 0), caller.Long(2)))

	if err := lower(caller, newCalleeConventions(m)); err != nil {
		t.Fatalf("lower: %v", err)
	}
	var conventions []bool
	for _, b := range caller.Blocks {
		for i := range b.Instrs {
			if in := &b.Instrs[i]; in.Op == ir.OCall {
				if !in.CallConvSet {
					t.Fatal("a lowered call left CallConvSet false; the emitter cannot tell the two apart")
				}
				conventions = append(conventions, callIsGoInternal(in))
			}
		}
	}
	if len(conventions) != 2 || !conventions[0] || conventions[1] {
		t.Errorf("lowered call conventions = %v, want [true false]", conventions)
	}
}

// TestGoInternalCallClobbersTheCallersCalleeSavedRegisters is the mixed-frame
// hazard. An ABIInternal callee preserves nothing; a System V caller believes RBX
// and R12..R15 survive a call and will happily park a value live across one in
// them. The lowered call has to say otherwise, or the value is destroyed.
func TestGoInternalCallClobbersTheCallersCalleeSavedRegisters(t *testing.T) {
	m := ir.NewModule()
	scalarFunc(m, "closure", ir.CallConvGoInternal, 1)
	caller := m.NewFunc("caller", ir.ClsL)
	e := caller.Entry()
	e.Ret(e.CallWithConvention(ir.ClsL, ir.CallConvGoInternal, caller.Sym("closure", 0), caller.Long(1)))

	if err := lower(caller, newCalleeConventions(m)); err != nil {
		t.Fatalf("lower: %v", err)
	}
	clobbered := map[Reg]bool{}
	for _, b := range caller.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op != ir.OCall {
				continue
			}
			for _, d := range in.Defs {
				if temp := caller.Temp(d); temp.Fixed {
					clobbered[Reg(temp.Reg)] = true
				}
			}
		}
	}
	for _, r := range []Reg{RBX, R12, R13, R14, R15} {
		if !clobbered[r] {
			t.Errorf("a Go ABIInternal call does not declare that it destroys %v, "+
				"which System V callee-saved rules would let the caller keep a value in", r)
		}
	}

	// The same-convention case must not pay for it: a System V call really does
	// preserve them, and declaring otherwise would spill every call-crossing value
	// in the C path.
	m2 := ir.NewModule()
	scalarFunc(m2, "plain", ir.CallConvPlatform, 1)
	plainCaller := m2.NewFunc("caller", ir.ClsL)
	pe := plainCaller.Entry()
	pe.Ret(pe.Call(ir.ClsL, plainCaller.Sym("plain", 0), plainCaller.Long(1)))
	if err := lower(plainCaller, newCalleeConventions(m2)); err != nil {
		t.Fatalf("lower: %v", err)
	}
	for _, b := range plainCaller.Blocks {
		for i := range b.Instrs {
			if in := &b.Instrs[i]; in.Op == ir.OCall && len(in.Defs) != 0 {
				t.Errorf("a System V call declares %d clobbered registers; it preserves them", len(in.Defs))
			}
		}
	}
}

// TestClosureContextIsCopiedOutOfRDXAtEntry covers the stabilization. RDX is
// ABIInternal's closure register and also the high half of div/rem and the
// widening multiply, so the pointer cannot stay there; the copy has to be the
// first thing the body does, before any instruction that needs RDX for its
// encoding.
func TestClosureContextIsCopiedOutOfRDXAtEntry(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("closure", ir.ClsL)
	f.CallConv = ir.CallConvGoInternal
	f.HasClosureContext = true
	p := f.Param("p", ir.ClsL)
	context := f.NewTemp("closure", ir.ClsL)
	ct := f.Temp(context)
	ct.Fixed, ct.Reg, ct.ClosureContext, ct.GCRef = true, int(regClosure), true, true
	e := f.Entry()
	e.Ret(e.Add(ir.ClsL, e.Div(ir.ClsL, p, f.Long(7)), context))

	if err := lower(f, newCalleeConventions(m)); err != nil {
		t.Fatalf("lower: %v", err)
	}

	// The copy is the first instruction after the parameter shuffle.
	at := 0
	for at < len(f.Start.Instrs) && f.Start.Instrs[at].Op == ir.OPar {
		at++
	}
	if at >= len(f.Start.Instrs) {
		t.Fatal("no instruction follows the parameter shuffle")
	}
	copyOut := f.Start.Instrs[at]
	if copyOut.Op != ir.OCopy || len(copyOut.Args) != 1 || copyOut.Args[0] != context {
		t.Fatalf("the first instruction after the parameter shuffle is %v, want a copy of the closure register",
			copyOut.Op)
	}
	saved := f.Temp(copyOut.To)
	if saved.Fixed {
		t.Errorf("the closure context was copied into another fixed register (%v); it must become allocatable",
			Reg(saved.Reg))
	}
	if !saved.GCRef {
		t.Error("the stabilized closure context lost its GC-root marking")
	}

	// And nothing else still reads the pinned register.
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			if i == at && b == f.Start {
				continue
			}
			for _, a := range b.Instrs[i].Args {
				if a == context {
					t.Errorf("instruction %v still reads the closure register directly", b.Instrs[i].Op)
				}
			}
		}
		if b.Jmp.Arg == context {
			t.Error("the terminator still reads the closure register directly")
		}
	}
}

// TestGoInternalRefusalsAreNamed checks that the ABIInternal shapes this backend
// cannot lower fail by name instead of producing something plausible.
func TestGoInternalRefusalsAreNamed(t *testing.T) {
	t.Run("variadic", func(t *testing.T) {
		m := ir.NewModule()
		f := m.NewFunc("closure", ir.ClsL)
		f.CallConv = ir.CallConvGoInternal
		f.Variadic = true
		e := f.Entry()
		e.Ret(f.Long(0))
		requireLowerError(t, m, f, "variadic")
	})

	t.Run("grouped aggregate parameters", func(t *testing.T) {
		m := ir.NewModule()
		f := m.NewFunc("closure", ir.ClsL)
		f.CallConv = ir.CallConvGoInternal
		f.Param("a", ir.ClsL)
		f.Param("b", ir.ClsL)
		f.ParamGroups = []ir.ValueGroup{{Index: 0, Count: 2,
			Type: &ir.AggType{Name: "g", Fields: []ir.Field{{Sub: ir.SubL}, {Sub: ir.SubL}}}}}
		e := f.Entry()
		e.Ret(f.Long(0))
		requireLowerError(t, m, f, "grouped aggregate parameters")
	})

	t.Run("result too large for the result registers", func(t *testing.T) {
		big := &ir.AggType{Name: "big", Fields: []ir.Field{{Sub: ir.SubL, Count: 40}}}
		m := ir.NewModule()
		m.AddType(big)
		f := m.NewFunc("closure", ir.ClsL)
		f.CallConv = ir.CallConvGoInternal
		f.RetAgg = big
		e := f.Entry()
		e.Ret(e.Alloc(8, 320))
		requireLowerError(t, m, f, "does not fit the result registers")
	})

	t.Run("cross-convention tail call", func(t *testing.T) {
		m := ir.NewModule()
		scalarFunc(m, "closure", ir.CallConvGoInternal, 1)
		f := m.NewFunc("caller", ir.ClsL)
		e := f.Entry()
		r := e.CallWithConvention(ir.ClsL, ir.CallConvGoInternal, f.Sym("closure", 0), f.Long(1))
		e.Instrs[len(e.Instrs)-1].Tail = true
		e.Ret(r)
		requireLowerError(t, m, f, "tail-call across calling conventions")
	})

	t.Run("cross-convention call reaching the caller's scratch pair", func(t *testing.T) {
		// ABIInternal argument registers 8 and 9 are R10/R11, which is exactly the
		// System V caller's emitter scratch pair.
		m := ir.NewModule()
		scalarFunc(m, "closure", ir.CallConvGoInternal, 9)
		f := m.NewFunc("caller", ir.ClsL)
		e := f.Entry()
		args := make([]ir.Ref, 9)
		for i := range args {
			args[i] = f.Long(int64(i))
		}
		e.Ret(e.CallWithConvention(ir.ClsL, ir.CallConvGoInternal, f.Sym("closure", 0), args...))
		requireLowerError(t, m, f, "emitter scratch register")
	})
}

func requireLowerError(t *testing.T, m *ir.Module, f *ir.Func, want string) {
	t.Helper()
	err := lower(f, newCalleeConventions(m))
	if err == nil {
		t.Fatalf("lowering %s succeeded; want an error mentioning %q", f.Name, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("lowering %s failed with %q, want it to mention %q", f.Name, err, want)
	}
}
