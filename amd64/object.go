package amd64

import (
	"fmt"
	"io"

	"github.com/evanphx/cg12/internal/gometa"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// CompileToObjectWith compiles a module to an in-memory relocatable object with
// the given options. A module carrying Plan 9 assembly needs its translated
// sidecar linked alongside; use WriteObjectAndAssembly for that.
func CompileToObjectWith(m *ir.Module, opts Options) (*obj.Object, error) {
	bundle, err := prepareAssembly(m)
	if err != nil {
		return nil, err
	}
	return compileToObjectWithBundle(m, opts, bundle)
}

// WriteObjectAndAssembly emits the cg12 ELF object directly to w and returns
// the GNU assembly translated from the module's Plan 9 sources, which the
// caller assembles into a second object and links alongside.
func WriteObjectAndAssembly(w io.Writer, m *ir.Module) (string, error) {
	bundle, err := prepareAssembly(m)
	if err != nil {
		return "", err
	}
	object, err := compileToObjectWithBundle(m, Options{}, bundle)
	if err != nil {
		return "", err
	}
	if err := object.WriteELF(w); err != nil {
		return "", err
	}
	return bundle.source, nil
}

// CompileToObjectAndAssembly builds the in-memory object for m and returns the
// translated assembly alongside it, for a caller that edits the object before
// serializing it.
func CompileToObjectAndAssembly(m *ir.Module) (*obj.Object, string, error) {
	bundle, err := prepareAssembly(m)
	if err != nil {
		return nil, "", err
	}
	object, err := compileToObjectWithBundle(m, Options{}, bundle)
	if err != nil {
		return nil, "", err
	}
	return object, bundle.source, nil
}

func compileToObjectWithBundle(m *ir.Module, opts Options, bundle assemblyBundle) (*obj.Object, error) {
	o := &obj.Object{Machine: obj.EM_X86_64}
	goRuntime := gometa.ModuleUsesGoRuntime(m)
	// Every call is lowered against its callee's convention, so the index of
	// what this object (and its assembly sidecar) defines is built once, before
	// the first function is touched.
	conventions := newCalleeConventions(m).withAssembly(bundle.callConventions)
	// Annotate managed functions with their stack pointer-word maps first: a
	// pure IR-to-IR pass that every later frame decision reads.
	for _, f := range m.Funcs {
		if f.UsesManagedFrame() {
			ir.InferStackPointerWords(f)
		}
	}
	var smFuncs []stackMapFunc
	var goFunctions []goFunctionInfo
	var rows []obj.LineRow
	var dfuncs []obj.DwarfFunc
	anchor := ""
	for _, f := range m.Funcs {
		name := sanitize(f.Name)
		params := dwarfParams(f) // captured before lowering rewrites the params
		paramTemps := paramTempIDs(f)
		ir.LowerPointers(f, ir.ClsL)
		applyAssemblyCallConventions(f, bundle.callConventions)
		if err := lower(f, conventions); err != nil {
			return nil, fmt.Errorf("function %s: %w", f.Name, err)
		}
		alloc, err := regAlloc(f)
		if err != nil {
			return nil, fmt.Errorf("function %s: %w", f.Name, err)
		}
		mc, err := emitMachine(f, alloc, opts.GC, opts.TLSModel)
		if err != nil {
			return nil, fmt.Errorf("function %s: %w", f.Name, err)
		}
		base := len(o.Text)
		if anchor == "" {
			anchor = name
		}
		o.Text = append(o.Text, mc.code...)
		for _, r := range mc.relocs {
			r.Offset += uint64(base)
			o.Relocs = append(o.Relocs, r)
		}
		o.Syms = append(o.Syms, obj.Sym{
			Name: name, Section: obj.SecText, Value: uint64(base),
			Size: uint64(len(mc.code)), Global: f.Linkage.Export || bundle.references[name], Func: true,
		})
		// Local symbols at address-taken blocks (&&label used in static data).
		for _, bs := range mc.blockSyms {
			o.Syms = append(o.Syms, obj.Sym{
				Name: bs.name, Section: obj.SecText, Value: uint64(base + bs.off),
			})
		}
		for i := range params {
			params[i].Loc = mc.m.varLoc(paramTemps[i])
		}
		df := obj.DwarfFunc{
			Name: name, Sym: name, Size: uint64(len(mc.code)),
			HasRet: f.HasRet, RetType: dwarfRetType(f), Params: params, External: f.Linkage.Export,
			Inlines: buildInlineTree(mc.inl, uint64(len(mc.code))),
		}
		if len(mc.rows) > 0 {
			df.DeclFile, df.DeclLine = mc.rows[0].File, mc.rows[0].Line
		}
		dfuncs = append(dfuncs, df)
		for _, r := range mc.rows {
			r.TextOff += uint64(base)
			rows = append(rows, r)
		}
		if goRuntime {
			info, err := goFunctionInfoFor(f, name, mc)
			if err != nil {
				return nil, fmt.Errorf("function %s: %w", f.Name, err)
			}
			goFunctions = append(goFunctions, info)
		} else if len(mc.safepoints) > 0 {
			// The legacy cg12 stack-map section is for non-Go collectors; a Go
			// module's maps are in pclntab.
			smFuncs = append(smFuncs, stackMapFunc{sym: name, points: mc.safepoints})
		}
	}
	if goRuntime {
		if err := finishGoModule(o, m, goFunctions, m.Data, bundle); err != nil {
			return nil, err
		}
	} else {
		for _, d := range m.Data {
			if bundle.definitions[sanitize(d.Name)] {
				continue
			}
			if err := addData(o, d); err != nil {
				return nil, err
			}
		}
	}
	if err := addAliases(o, m.Aliases); err != nil {
		return nil, err
	}
	if len(m.Files) > 0 && anchor != "" {
		o.SetDWARF(m.Files, rows, dfuncs, uint64(len(o.Text)), anchor, "cg12", ".", m.Files[0], obj.R_X86_64_64, 0x56) // DW_OP_reg6 (rbp)
	}
	if len(smFuncs) > 0 {
		setStackMap(o, smFuncs)
	}
	for index := range o.Syms {
		if bundle.references[o.Syms[index].Name] {
			o.Syms[index].Global = true
		}
	}
	return o, nil
}
