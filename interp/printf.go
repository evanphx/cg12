package interp

import (
	"strconv"
	"strings"
)

// cPrintf renders a C printf format the way libc would, pulling arguments from
// next (which reports ok=false when the format asks for more than were supplied).
// It is a documented subset — the conversions and flags the corpus and typical
// programs use — and traps on anything outside it rather than approximating.
func (mc *Machine) cPrintf(format []byte, next func() (Value, bool)) (string, error) {
	var out []byte
	arg := func() (Value, error) {
		v, ok := next()
		if !ok {
			return Value{}, mc.trapf("printf: format consumes more arguments than were passed")
		}
		return v, nil
	}

	for i := 0; i < len(format); {
		if format[i] != '%' {
			out = append(out, format[i])
			i++
			continue
		}
		i++ // consume '%'
		// Flags.
		flags := ""
		for i < len(format) && strings.IndexByte("-+ 0#", format[i]) >= 0 {
			flags += string(format[i])
			i++
		}
		// Width.
		ws := i
		for i < len(format) && format[i] >= '0' && format[i] <= '9' {
			i++
		}
		width := 0
		if i > ws {
			width, _ = strconv.Atoi(string(format[ws:i]))
		}
		// Precision (only .N; used for floats).
		prec := -1
		if i < len(format) && format[i] == '.' {
			i++
			ps := i
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				i++
			}
			prec, _ = strconv.Atoi(string(format[ps:i]))
		}
		// Length modifiers (ignored — a Value already carries its width).
		for i < len(format) && strings.IndexByte("lhzjt", format[i]) >= 0 {
			i++
		}
		if i >= len(format) {
			return "", mc.trapf("printf: format ends after %%")
		}
		verb := format[i]
		i++

		if verb == '%' {
			out = append(out, '%')
			continue
		}
		v, err := arg()
		if err != nil {
			return "", err
		}
		s, err := mc.formatVerb(verb, v, prec)
		if err != nil {
			return "", err
		}
		out = append(out, pad(s, flags, width, isNumericVerb(verb))...)
	}
	return string(out), nil
}

func (mc *Machine) formatVerb(verb byte, v Value, prec int) (string, error) {
	fprec := prec
	if fprec < 0 {
		fprec = 6 // C default for f/e/g
	}
	switch verb {
	case 'd', 'i':
		return strconv.FormatInt(v.i64(), 10), nil
	case 'u':
		return strconv.FormatUint(v.u64(), 10), nil
	case 'x':
		return strconv.FormatUint(v.u64(), 16), nil
	case 'X':
		return strings.ToUpper(strconv.FormatUint(v.u64(), 16)), nil
	case 'o':
		return strconv.FormatUint(v.u64(), 8), nil
	case 'c':
		return string([]byte{byte(v.i64())}), nil
	case 'p':
		return "0x" + strconv.FormatUint(v.u64(), 16), nil
	case 's':
		b, ok := mc.readCString(v.u64())
		if !ok {
			return "", mc.trapf("printf %%s: string at %#x unmapped", v.u64())
		}
		return string(b), nil
	case 'f', 'F':
		return strconv.FormatFloat(v.f64(), 'f', fprec, 64), nil
	case 'e', 'E':
		return strconv.FormatFloat(v.f64(), verb, fprec, 64), nil
	case 'g', 'G':
		p := prec
		if p < 0 {
			p = 6 // C %g default significant digits
		}
		return strconv.FormatFloat(v.f64(), verb|0x20, p, 64), nil // 'g'/'G'
	}
	return "", mc.trapf("printf: unsupported conversion %%%c", verb)
}

func isNumericVerb(verb byte) bool {
	return strings.IndexByte("diuxXofFeEgG", verb) >= 0
}

// pad applies field width: right-justified with spaces (or zeros for a numeric
// conversion with the 0 flag), or left-justified with the - flag.
func pad(s, flags string, width int, numeric bool) string {
	if len(s) >= width {
		return s
	}
	n := width - len(s)
	if strings.Contains(flags, "-") {
		return s + strings.Repeat(" ", n)
	}
	if numeric && strings.Contains(flags, "0") {
		// Keep a leading sign ahead of the zeros.
		if len(s) > 0 && (s[0] == '-' || s[0] == '+') {
			return s[:1] + strings.Repeat("0", n) + s[1:]
		}
		return strings.Repeat("0", n) + s
	}
	return strings.Repeat(" ", n) + s
}
