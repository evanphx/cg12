package arm64

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
	plan9sem "github.com/evanphx/cg12/plan9asm/sem"
)

func lowerSemanticData(source *plan9sem.Data) (*ir.Data, error) {
	data := &ir.Data{
		Name: source.Sym.Name,
		Linkage: ir.Linkage{
			Export: !source.Sym.Static,
			Thread: source.Thread,
		},
		Align: 1,
	}
	if source.ReadOnly {
		data.Linkage.Section = rodataSection
	}

	cursor := 0
	for _, item := range source.Items {
		if item.Offset < cursor || item.Offset+item.Width > source.Size {
			return nil, fmt.Errorf("initializer %d/%d is outside %s", item.Offset, item.Width, source.Sym.Name)
		}
		if item.Offset > cursor {
			data.Items = append(data.Items, ir.DataItem{Zero: item.Offset - cursor})
		}
		if item.Width > data.Align && item.Width <= 16 {
			data.Align = item.Width
		}

		subclass, err := semanticDataSubclass(item.Width)
		if err != nil {
			return nil, err
		}
		switch {
		case item.Symbol != nil:
			data.Items = append(data.Items, ir.DataItem{
				Sub: subclass,
				Sym: item.Symbol.Name,
				Off: item.Addend,
			})
			if !source.NoPointers && item.Width == 8 && item.Offset%8 == 0 {
				data.PointerWords = append(data.PointerWords, item.Offset/8)
			}
		case item.Bytes != nil:
			data.Items = append(data.Items, ir.DataItem{Str: string(item.Bytes)})
			if padding := item.Width - len(item.Bytes); padding > 0 {
				data.Items = append(data.Items, ir.DataItem{Zero: padding})
			}
		default:
			data.Items = append(data.Items, ir.DataItem{Sub: subclass, Ints: []int64{item.Value}})
		}
		cursor = item.Offset + item.Width
	}
	if cursor < source.Size {
		data.Items = append(data.Items, ir.DataItem{Zero: source.Size - cursor})
	}
	return data, nil
}

func semanticDataSubclass(width int) (ir.SubCls, error) {
	switch width {
	case 1:
		return ir.SubUB, nil
	case 2:
		return ir.SubUH, nil
	case 4:
		return ir.SubW, nil
	case 8:
		return ir.SubL, nil
	}
	return 0, fmt.Errorf("semantic data width %d is unsupported", width)
}
