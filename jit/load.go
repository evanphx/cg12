//go:build linux && (amd64 || arm64)

package jit

import (
	"encoding/binary"
	"fmt"
	"syscall"

	"github.com/evanphx/cg12/obj"
)

// Code is an object laid out in executable memory: its text and read-only
// data share one mapping that is written, then made read+execute, so no
// page is ever writable and executable at once. Writable data, which the
// lowering never asks for, gets a mapping of its own.
type Code struct {
	text []byte // text and rodata, read+execute
	data []byte // data and bss, read+write
	syms map[string]uintptr
}

// Addr returns a symbol's address, or 0 when the object doesn't define it.
func (c *Code) Addr(name string) uintptr { return c.syms[name] }

// Free unmaps the code. Nothing may be running in it.
func (c *Code) Free() {
	if c.text != nil {
		syscall.Munmap(c.text)
	}
	if c.data != nil {
		syscall.Munmap(c.data)
	}
	c.text, c.data = nil, nil
}

func align(n, a int) int {
	if a <= 1 {
		return n
	}
	return (n + a - 1) / a * a
}

// Load lays o out in fresh memory, resolves its relocations, and protects
// it. resolve gives the address of a symbol o refers to but doesn't
// define; the lowering refers to none, so a nil resolve is fine.
func Load(o *obj.Object, resolve func(string) (uintptr, bool)) (*Code, error) {
	if o.Machine != machine {
		return nil, fmt.Errorf("jit: object is for machine %d, not this one", o.Machine)
	}
	if len(o.Tdata) > 0 || o.TbssSize > 0 {
		return nil, fmt.Errorf("jit: thread-local data is unsupported")
	}
	page := syscall.Getpagesize()
	rodataOff := align(len(o.Text), max(o.RodataAlign, 16))
	relroOff := align(rodataOff+len(o.Rodata), max(o.RelroAlign, 16))
	textSize := align(relroOff+len(o.Relro), page)
	bssOff := align(len(o.Data), max(o.BssAlign, 16))
	dataSize := align(bssOff+o.BssSize, page)

	c := &Code{syms: map[string]uintptr{}}
	var err error
	c.text, err = syscall.Mmap(-1, 0, max(textSize, page), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("jit: mapping code: %w", err)
	}
	if dataSize > 0 {
		c.data, err = syscall.Mmap(-1, 0, dataSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
		if err != nil {
			c.Free()
			return nil, fmt.Errorf("jit: mapping data: %w", err)
		}
	}
	copy(c.text, o.Text)
	copy(c.text[rodataOff:], o.Rodata)
	copy(c.text[relroOff:], o.Relro)
	if c.data != nil {
		copy(c.data, o.Data)
	}

	textBase := addrOf(c.text)
	var dataBase uintptr
	if c.data != nil {
		dataBase = addrOf(c.data)
	}
	secBase := func(s obj.SecKind) (uintptr, error) {
		switch s {
		case obj.SecText:
			return textBase, nil
		case obj.SecRodata:
			return textBase + uintptr(rodataOff), nil
		case obj.SecRelro:
			return textBase + uintptr(relroOff), nil
		case obj.SecData:
			return dataBase, nil
		case obj.SecBss:
			return dataBase + uintptr(bssOff), nil
		}
		return 0, fmt.Errorf("jit: symbols in section %d are unsupported", s)
	}
	for _, s := range o.Syms {
		if s.Section == obj.SecUndef || s.Name == "" {
			continue
		}
		b, err := secBase(s.Section)
		if err != nil {
			c.Free()
			return nil, err
		}
		c.syms[s.Name] = b + uintptr(s.Value)
	}
	symAddr := func(name string) (uintptr, error) {
		if a, ok := c.syms[name]; ok {
			return a, nil
		}
		if resolve != nil {
			if a, ok := resolve(name); ok {
				return a, nil
			}
		}
		return 0, fmt.Errorf("jit: undefined symbol %q", name)
	}
	apply := func(mem []byte, base uintptr, rs []obj.Reloc) error {
		for _, r := range rs {
			s, err := symAddr(r.Sym)
			if err != nil {
				return err
			}
			if err := relocate(mem, base, r, s); err != nil {
				return err
			}
		}
		return nil
	}
	if err := apply(c.text, textBase, o.Relocs); err == nil {
		if err = apply(c.text[rodataOff:], textBase+uintptr(rodataOff), o.RodataRelocs); err == nil {
			if err = apply(c.text[relroOff:], textBase+uintptr(relroOff), o.RelroRelocs); err == nil && c.data != nil {
				err = apply(c.data, dataBase, o.DataRelocs)
			}
		}
		if err != nil {
			c.Free()
			return nil, err
		}
	} else {
		c.Free()
		return nil, err
	}
	if err := syscall.Mprotect(c.text, syscall.PROT_READ|syscall.PROT_EXEC); err != nil {
		c.Free()
		return nil, fmt.Errorf("jit: protecting code: %w", err)
	}
	flushICache(textBase, uintptr(len(c.text)))
	return c, nil
}

// relocate patches one relocation at r.Offset in mem, which starts at base.
func relocate(mem []byte, base uintptr, r obj.Reloc, sym uintptr) error {
	at := int(r.Offset)
	if at < 0 || at+4 > len(mem) {
		return fmt.Errorf("jit: relocation to %q is outside its section", r.Sym)
	}
	p := int64(base) + int64(r.Offset)
	if err := relocateArch(mem[at:], p, int64(sym)+r.Addend, r.Type); err != nil {
		return fmt.Errorf("jit: relocation to %q: %w", r.Sym, err)
	}
	return nil
}

// fits reports whether v fits in a signed field of n bits.
func fits(v int64, n uint) bool { return v >= -(1<<(n-1)) && v < 1<<(n-1) }

var errRange = fmt.Errorf("out of range")

func put32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }

// put64 writes a 64-bit word, which relocate's bounds check doesn't
// cover.
func put64(b []byte, v uint64) error {
	if len(b) < 8 {
		return fmt.Errorf("outside its section")
	}
	binary.LittleEndian.PutUint64(b, v)
	return nil
}
