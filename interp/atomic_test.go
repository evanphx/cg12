package interp_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Atomics are intrinsics. In the single-threaded interpreter each is an ordinary
// load/modify/store, so these check the semantics (and that both engines agree)
// rather than any concurrency. Each program allocates a scratch cell, drives it
// through the atomic operations, and returns a value that encodes every step.
func TestAtomics(t *testing.T) {
	cases := []struct {
		name string
		il   string
		want int64
	}{
		{
			name: "load-store-long",
			il: `export function l $f() {
				@s
				%p =l alloc8 8
				intrinsic atomic.store.l %p, 42
				%v =l intrinsic atomic.load.l %p
				ret %v
			}`,
			want: 42,
		},
		{
			name: "load-store-word",
			il: `export function w $f() {
				@s
				%p =l alloc4 4
				intrinsic atomic.store.w %p, 7
				%v =w intrinsic atomic.load.w %p
				ret %v
			}`,
			want: 7,
		},
		{
			name: "xchg",
			il: `export function l $f() {
				@s
				%p =l alloc8 8
				intrinsic atomic.store.l %p, 10
				%old =l intrinsic atomic.xchg.l %p, 20
				%new =l intrinsic atomic.load.l %p
				%r =l add %old, %new
				ret %r
			}`,
			want: 30, // old 10 + new 20
		},
		{
			name: "cas-success-then-fail",
			il: `export function l $f() {
				@s
				%p =l alloc8 8
				intrinsic atomic.store.l %p, 5
				%a =l intrinsic atomic.cas.l %p, 5, 7
				%b =l intrinsic atomic.cas.l %p, 5, 9
				%v =l intrinsic atomic.load.l %p
				%s =l add %a, %b
				%r =l add %s, %v
				ret %r
			}`,
			want: 19, // a=5 (swapped), b=7 (no swap), v=7
		},
		{
			name: "rmw-add-sub",
			il: `export function l $f() {
				@s
				%p =l alloc8 8
				intrinsic atomic.store.l %p, 100
				%o1 =l intrinsic atomic.add.l %p, 5
				%o2 =l intrinsic atomic.sub.l %p, 10
				%v =l intrinsic atomic.load.l %p
				%s =l add %o1, %o2
				%r =l add %s, %v
				ret %r
			}`,
			want: 300, // o1=100, o2=105, v=95
		},
		{
			name: "rmw-bitwise",
			il: `export function l $f() {
				@s
				%p =l alloc8 8
				intrinsic atomic.store.l %p, 12
				%o1 =l intrinsic atomic.or.l %p, 1
				%o2 =l intrinsic atomic.and.l %p, 6
				%o3 =l intrinsic atomic.xor.l %p, 15
				%v =l intrinsic atomic.load.l %p
				ret %v
			}`,
			want: 11, // store 12; or 1 -> 13; and 6 -> 4; xor 15 -> 11
		},
		{
			name: "fence",
			il: `export function l $f() {
				@s
				%p =l alloc8 8
				intrinsic atomic.store.l %p, 1
				intrinsic atomic.fence
				%v =l intrinsic atomic.load.l %p
				ret %v
			}`,
			want: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tw := run(t, c.il, "f")
			bc := runBC(t, c.il, "f")
			require.Equal(t, tw, bc, "tree-walker vs bytecode")
			require.Equal(t, c.want, tw.I64())
		})
	}
}
