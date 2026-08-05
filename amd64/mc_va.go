package amd64

import (
	"fmt"

	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
)

// namedCounts replays argument assignment over the named parameters, returning
// how many arrived in GP and SSE registers and the byte size of the named stack
// arguments — the starting state for a va_list.
func namedCounts(f *ir.Func) (ngp, nfp, stackBytes int) {
	a := newArgAssigner(false)
	for _, p := range f.Params {
		if p.Agg != nil {
			a.assignAgg(classifyAgg(p.Agg))
			continue
		}
		a.assign(p.Cls)
	}
	return a.ngrn, a.nsrn, roundUp(a.nsaa, 8)
}

// vaStart initializes a System V va_list at the buffer pointed to by Args[0]:
// {gp_offset u32, fp_offset u32, overflow_arg_area ptr, reg_save_area ptr}.
func (m *mc) vaStart(in *ir.Instr) {
	m.gpInto(m.gpScratch0, in.Arg(0)) // scratch0 = &va_list
	ap := m.gpScratch0.mreg()
	ngp, nfp, stackBytes := namedCounts(m.f)

	m.emit(x64.StoreImm32(32, x64.At(ap, 0), int32(8*ngp)))            // gp_offset
	m.emit(x64.StoreImm32(32, x64.At(ap, 4), int32(vaGPBytes+16*nfp))) // fp_offset
	m.emit(x64.Lea(true, m.gpScratch1.mreg(), x64.At(RBP.mreg(), int32(16+stackBytes))))
	m.emit(x64.Store(64, m.gpScratch1.mreg(), x64.At(ap, 8))) // overflow_arg_area
	m.emit(x64.Lea(true, m.gpScratch1.mreg(), x64.At(RBP.mreg(), int32(-m.regSaveDist))))
	m.emit(x64.Store(64, m.gpScratch1.mreg(), x64.At(ap, 16))) // reg_save_area
}

// vaArg fetches the next variadic argument of the requested class, advancing the
// va_list. It mirrors the System V algorithm: use the register save area while an
// eightbyte remains there, else the overflow (stack) area. Uses r10 (the va_list
// pointer), r11 (offset scratch), and rdx (address) — all reserved from
// allocation, and none needed by the surrounding code at a va_arg.
func (m *mc) vaArg(in *ir.Instr) {
	float := in.Cls.IsFloat()
	offField, bound, step := int32(0), int32(vaGPBytes), int32(8)
	if float {
		offField, bound, step = 4, int32(vaRegSaveSz), 16
	}

	m.gpInto(m.gpScratch0, in.Arg(0)) // scratch0 = &va_list
	ap := m.gpScratch0.mreg()
	off, addr := m.gpScratch1.mreg(), RDX.mreg()

	m.vaSeq++
	over := fmt.Sprintf("va%d_over", m.vaSeq)
	join := fmt.Sprintf("va%d_join", m.vaSeq)

	m.emit(x64.Load(false, off, x64.At(ap, offField))) // off = gp_offset / fp_offset
	m.emit(x64.CmpImm(false, off, bound))
	m.prog.Jcc(x64.AE, over)

	// Register save area: addr = reg_save_area + off; then advance the offset.
	m.emit(x64.Load(true, addr, x64.At(ap, 16)))
	m.emit(x64.AddReg(true, addr, off))
	m.emit(x64.AddImm(false, off, step))
	m.emit(x64.Store(32, off, x64.At(ap, offField)))
	m.prog.Jmp(join)

	// Overflow (stack) area: addr = overflow_arg_area; then advance it by 8.
	m.prog.Label(over)
	m.emit(x64.Load(true, addr, x64.At(ap, 8)))
	m.emit(x64.Lea(true, off, x64.At(addr, 8)))
	m.emit(x64.Store(64, off, x64.At(ap, 8)))

	m.prog.Label(join)
	if float {
		d, commit := m.fpDst(in.To)
		if in.Cls == ir.ClsD {
			m.emit(x64.MovsdLoad(d.mreg(), x64.At(addr, 0)))
		} else {
			m.emit(x64.MovssLoad(d.mreg(), x64.At(addr, 0)))
		}
		commit()
		return
	}
	d, commit := m.gpDst(in.To)
	m.emit(x64.Load(in.Cls == ir.ClsL, d.mreg(), x64.At(addr, 0)))
	commit()
}
