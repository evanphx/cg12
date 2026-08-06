package memo

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

// The memo file is one flat, versioned, self-describing sequence of entries.
//
// One file rather than one file per function because this is a measurement
// implementation and a single file makes "what does the memo cost on disk" a
// question with one answer. A production cache would want gc's shape -- one file
// per entry under a two-hex-digit fanout, so two compiles can write concurrently
// and an eviction can unlink one entry -- and that is a change to this file
// alone.
const (
	memoMagic   = "cg12memo"
	memoVersion = 1
)

func sortStrings(s []string) { sort.Strings(s) }

// Save writes the store.
func (s *Store) Save(path string) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(out, 1<<20)
	if err := s.writeTo(w); err != nil {
		out.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (s *Store) writeTo(w *bufio.Writer) error {
	if _, err := w.WriteString(memoMagic); err != nil {
		return err
	}
	var scratch [binary.MaxVarintLen64]byte
	putUvarint := func(v uint64) error {
		n := binary.PutUvarint(scratch[:], v)
		_, err := w.Write(scratch[:n])
		return err
	}
	putBytes := func(b []byte) error {
		if err := putUvarint(uint64(len(b))); err != nil {
			return err
		}
		_, err := w.Write(b)
		return err
	}
	putString := func(v string) error { return putBytes([]byte(v)) }
	putBool := func(v bool) error {
		if v {
			return w.WriteByte(1)
		}
		return w.WriteByte(0)
	}

	if err := putUvarint(memoVersion); err != nil {
		return err
	}
	if _, err := w.Write(s.Data[:]); err != nil {
		return err
	}
	names := make([]string, 0, len(s.Entries))
	for name := range s.Entries {
		names = append(names, name)
	}
	sort.Strings(names)
	if err := putUvarint(uint64(len(names))); err != nil {
		return err
	}
	for _, name := range names {
		e := s.Entries[name]
		if err := putString(e.Name); err != nil {
			return err
		}
		for _, d := range [][]byte{e.Input[:], e.Module[:], e.Output[:]} {
			if _, err := w.Write(d); err != nil {
				return err
			}
		}
		if err := putBool(e.Recursive); err != nil {
			return err
		}
		if err := putBool(e.Dropped); err != nil {
			return err
		}
		if err := putBytes(e.Body); err != nil {
			return err
		}
		if err := putUvarint(uint64(len(e.Deps))); err != nil {
			return err
		}
		for _, d := range e.Deps {
			if err := putString(d.Name); err != nil {
				return err
			}
			if _, err := w.Write(d.Input[:]); err != nil {
				return err
			}
			if err := putBool(d.Recursive); err != nil {
				return err
			}
		}
		if err := putUvarint(uint64(len(e.Spliced))); err != nil {
			return err
		}
		for _, name := range e.Spliced {
			if err := putString(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// Load reads a store. A missing file is an empty store, not an error: that is a
// cold build.
func Load(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewStore(), nil
	}
	if err != nil {
		return nil, err
	}
	store := NewStore()
	r := &reader{data: data}
	if got := r.bytes(len(memoMagic)); string(got) != memoMagic {
		return nil, fmt.Errorf("memo: %s is not a memo file", path)
	}
	if v := r.uvarint(); v != memoVersion {
		// A stale format is a cold build, not a failure -- the same rule gc's
		// cache uses, and the same rule ir's unit format uses.
		return NewStore(), nil
	}
	copy(store.Data[:], r.bytes(32))
	count := r.uvarint()
	for i := uint64(0); i < count; i++ {
		e := &Entry{}
		e.Name = r.str()
		copy(e.Input[:], r.bytes(32))
		copy(e.Module[:], r.bytes(32))
		copy(e.Output[:], r.bytes(32))
		e.Recursive = r.boolean()
		e.Dropped = r.boolean()
		e.Body = r.blob()
		deps := r.uvarint()
		e.Deps = make([]Dep, 0, deps)
		for j := uint64(0); j < deps; j++ {
			var d Dep
			d.Name = r.str()
			copy(d.Input[:], r.bytes(32))
			d.Recursive = r.boolean()
			e.Deps = append(e.Deps, d)
		}
		spliced := r.uvarint()
		e.Spliced = make([]string, 0, spliced)
		for j := uint64(0); j < spliced; j++ {
			e.Spliced = append(e.Spliced, r.str())
		}
		if r.err != nil {
			return nil, fmt.Errorf("memo: %s: %w", path, r.err)
		}
		store.Put(e)
	}
	return store, nil
}

type reader struct {
	data []byte
	pos  int
	err  error
}

func (r *reader) bytes(n int) []byte {
	if r.err != nil || r.pos+n > len(r.data) {
		r.err = io.ErrUnexpectedEOF
		return make([]byte, n)
	}
	out := r.data[r.pos : r.pos+n]
	r.pos += n
	return out
}

func (r *reader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.data[r.pos:])
	if n <= 0 {
		r.err = io.ErrUnexpectedEOF
		return 0
	}
	r.pos += n
	return v
}

func (r *reader) blob() []byte {
	n := r.uvarint()
	b := r.bytes(int(n))
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func (r *reader) str() string { return string(r.blob()) }

func (r *reader) boolean() bool { return r.bytes(1)[0] != 0 }
