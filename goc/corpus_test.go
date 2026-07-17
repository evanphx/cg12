package goc_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

// TestExecutionCorpus climbs from expressions to stateful control flow. Every
// case passes through the Go parser/type checker, cg12 IR, native machine-code
// emitter, ELF writer, system linker, and finally the host CPU.
func TestExecutionCorpus(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"constants", `func Test() int { return 42 }`, 42},
		{"arithmetic precedence", `func Test() int { return 2 + 3*4 - 8/2 }`, 10},
		{"signed division and remainder", `func Test() int { return (-17/5)*10 + (-17%5) }`, -32},
		{"unsigned arithmetic", `func Test() int { var x uint = 1<<63; return int((x>>62) + x/x) }`, 3},
		{"bitwise", `func Test() int { return (0x55 & 0x0f) | (3 << 4) ^ 2 }`, 55},
		{"variable shift at word width", `func Test() int { n := uint(64); return int((uint64(1) << n) + (uint64(7) >> n)) }`, 0},
		{"variable signed shift at word width", `func Test() int { n := uint(64); return int(int64(-7) >> n) }`, -1},
		{"variable shift beyond word width", `func Test() int { n := uint64(65); return int((uint64(1) << n) + (uint64(7) >> n)) }`, 0},
		{"variable 32-bit shift at width", `func Test() int { n := uint64(32); return int((uint32(1) << n) + (uint32(7) >> n)) }`, 0},
		{"stdlib 64-bit population count", `func onesCount64(x uint64) int {
			const m0 = 0x5555555555555555
			const m1 = 0x3333333333333333
			const m2 = 0x0f0f0f0f0f0f0f0f
			x = x>>1&m0 + x&m0
			x = x>>2&m1 + x&m1
			x = (x>>4 + x) & m2
			x += x >> 8
			x += x >> 16
			x += x >> 32
			return int(x) & 127
		}; func Test() int { return onesCount64(0x7f) + 10*onesCount64(^uint64(0)) }`, 647},
		{"stdlib bit length table", `const lengths = "\x00\x01\x02\x02\x03\x03\x03\x03\x04\x04\x04\x04\x04\x04\x04\x04"
			func bitLength(x uint64) (n int) {
				if x >= 1<<32 { x >>= 32; n = 32 }
				if x >= 1<<16 { x >>= 16; n += 16 }
				if x >= 1<<8 { x >>= 8; n += 8 }
				return n + int(lengths[uint8(x)])
			}
			func Test() int { return bitLength(0x0fffffffffffffff) + bitLength(0xf) + bitLength(0) }`, 64},
		{"runtime bit range search", `func trailingZeros64(x uint64) int {
			if x == 0 { return 64 }
			n := 0
			for x&1 == 0 { n++; x >>= 1 }
			return n
		}; func findBitRange64(c uint64, n uint) uint {
			p := n - 1
			k := uint(1)
			for p > 0 {
				if p <= k { c &= c >> (p & 63); break }
				c &= c >> (k & 63)
				if c == 0 { return 64 }
				p -= k
				k *= 2
			}
			return uint(trailingZeros64(c))
		}; func Test() int {
			return int(findBitRange64(0x78, 3))*100 + int(findBitRange64(0x8000000007ffffff, 4))*10 + int(findBitRange64(0x00f0f000, 5))
		}`, 364},
		{"runtime page bitmap ranges", `type pageBits [8]uint64
			func (b *pageBits) setRange(i, n uint) {
				j := i + n - 1
				if i/64 == j/64 {
					b[i/64] |= ((uint64(1) << n) - 1) << (i % 64)
					return
				}
				b[i/64] |= ^uint64(0) << (i % 64)
				for k := i/64 + 1; k < j/64; k++ { b[k] = ^uint64(0) }
				b[j/64] |= (uint64(1) << (j%64 + 1)) - 1
			}
			func (b *pageBits) clearRange(i, n uint) {
				j := i + n - 1
				if i/64 == j/64 {
					b[i/64] &^= ((uint64(1) << n) - 1) << (i % 64)
					return
				}
				b[i/64] &^= ^uint64(0) << (i % 64)
				clear(b[i/64+1:j/64])
				b[j/64] &^= (uint64(1) << (j%64 + 1)) - 1
			}
			func Test() int {
				var bits pageBits
				bits.setRange(0, 512)
				bits.clearRange(9, 7)
				score := 0
				for i, word := range bits {
					if i == 0 {
						if word == 0xffffffffffff01ff { score++ }
					} else if word == ^uint64(0) { score++ }
				}
				return score
			}`, 8},
		{"runtime nested scavenged bitmap", `type pageBits [8]uint64
			type pageData struct { allocated pageBits; scavenged pageBits }
			func (b *pageBits) setRange(i, n uint) {
				j := i + n - 1
				b[i/64] |= ^uint64(0) << (i % 64)
				for k := i/64 + 1; k < j/64; k++ { b[k] = ^uint64(0) }
				b[j/64] |= (uint64(1) << (j%64 + 1)) - 1
			}
			func fill(data *pageData) { data.scavenged.setRange(0, 512) }
			func Test() int {
				var data pageData
				fill(&data)
				score := 0
				for _, word := range data.scavenged { if word == ^uint64(0) { score++ } }
				return score
			}`, 8},
		{"runtime page bitmap population range", `type pageBits [8]uint64
			func ones(x uint64) uint {
				var count uint
				for x != 0 { x &= x - 1; count++ }
				return count
			}
			func (b *pageBits) popcntRange(i, n uint) uint {
				j := i + n - 1
				if i/64 == j/64 {
					return ones((b[i/64] >> (i % 64)) & ((1 << n) - 1))
				}
				return 0
			}
			func Test() int {
				bits := pageBits{^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)}
				return int(bits.popcntRange(376, 7))
			}`, 7},
		{"returned three-word runtime cache", `type cache struct { base, free, scav uint64 }
			func acquire() cache { return cache{base: 11, free: 22, scav: 33} }
			func fill(c *cache) { *c = acquire() }
			func Test() int { var c cache; fill(&c); return int(c.base + c.free + c.scav) }`, 66},
		{"runtime page cache scavenged result", `type pageCache struct { base uintptr; cache, scav uint64 }
			func acquire() pageCache { return pageCache{base: 0x100000, cache: ^uint64(0), scav: ^uint64(0)} }
			func (c *pageCache) alloc(npages uintptr) (uintptr, uintptr) {
				mask := (uint64(1) << npages) - 1
				scav := uint64(0)
				for bits := c.scav & mask; bits != 0; bits &= bits - 1 { scav++ }
				c.cache &^= mask
				c.scav &^= mask
				return c.base, uintptr(scav) * 8192
			}
			func Test() int {
				var cache pageCache
				cache = acquire()
				base, scavenged := cache.alloc(7)
				if base != 0x100000 || scavenged != 7*8192 { return 0 }
				if cache.cache != 0xffffffffffffff80 || cache.scav != 0xffffffffffffff80 { return 0 }
				return 42
			}`, 42},
		{"clear pointer array subslice", `func clearMiddle(values *[8]uint64) { clear(values[1:7]) }; func Test() int {
			values := [8]uint64{1, 2, 4, 8, 16, 32, 64, 128}
			clearMiddle(&values)
			return int(values[0] + values[1] + values[6] + values[7])
		}`, 129},
		{"signed byte overflow", `func Test() int { var x int8=127; x++; return int(x) }`, -128},
		{"unsigned byte overflow", `func Test() int { var x uint8=255; x+=2; return int(x) }`, 1},
		{"word overflow", `func Test() int { var x uint32=0xffffffff; x++; return int(x) }`, 0},
		{"comparisons", `func Test() int { n:=0; if -1 < 1 { n+=1 }; if uint(1) < uint(2) { n+=2 }; if 3 != 4 { n+=4 }; return n }`, 7},
		{"string value equality", `func Test() int { text := "abc"; copy := text[:]; var empty string; score := 0; if text == copy { score += 1 }; if empty == "" { score += 2 }; if text != "abd" { score += 4 }; return score }`, 7},
		{"string lexical ordering", `func Test() int {
			higher := "z"
			lower := "a"
			score := 0
			if higher > lower {
				score += 1
			}
			if higher >= "z" {
				score += 2
			}
			if lower < higher {
				score += 4
			}
			if lower <= "a" {
				score += 8
			}
			if "ab" < "aba" {
				score += 16
			}
			if "aba" > "ab" {
				score += 32
			}
			return score
		}`, 63},
		{"float comparisons", `func Test() int { a, b := 1.5, 2.5; if a < b && b >= a && a != b { return 42 }; return 0 }`, 42},
		{"locals and compound assignment", `func Test() int { x:=3; x+=4; x*=5; x-=2; return x }`, 33},
		{"parallel assignment", `func Test() int { x,y:=3,8; x,y=y,x; return x*10+y }`, 83},
		{"lexical shadowing", `func Test() int { x:=2; { x:=9; x++ }; return x }`, 2},
		{"if init and else", `func Test() int { if x:=7; x>8 { return 1 } else if x==7 { return 2 }; return 3 }`, 2},
		{"for loop", `func Test() int { s:=0; for i:=1; i<=10; i++ { s+=i }; return s }`, 55},
		{"break and continue", `func Test() int { s:=0; for i:=0; ; i++ { if i==8 { break }; if i%2==0 { continue }; s+=i }; return s }`, 16},
		{"labeled continue", `func Test() int { s:=0; outer: for i:=0; i<5; i++ { for j:=0; j<5; j++ { if j==2 { continue outer }; s += i*10+j } }; return s }`, 205},
		{"labeled break", `func Test() int { n:=0; outer: for i:=0; i<6; i++ { for j:=0; j<5; j++ { if i==3 && j==2 { break outer }; n++ } }; return n }`, 17},
		{"function call", `func twice(x int) int { return x*2 }; func Test() int { return twice(21) }`, 42},
		{"recursion", `func fib(n int) int { if n<2 { return n }; return fib(n-1)+fib(n-2) }; func Test() int { return fib(10) }`, 55},
		{"range array", `func Test() int { values := [4]int{2, 3, 5, 7}; sum := 0; for _, value := range values { sum += value }; return sum }`, 17},
		{"range pointer to array", `func Test() int { values := [4]int{2, 3, 5, 7}; sum := 0; for _, value := range &values { sum += value }; return sum }`, 17},
		{"slice pointer to array", `func Test() int { values := [4]int{2, 3, 5, 7}; slice := (&values)[:]; return len(slice)*10 + slice[2] }`, 45},
		{"pointer to slice descriptor", `func measure(values *[]int) int { return len(*values)*10 + cap(*values) }; func Test() int { values := []int{2, 3, 5}; return measure(&values) }`, 33},
		{"reslice preserves capacity", `func Test() int { values := []int{2, 3, 5}; values = values[:1]; return len(values)*10 + cap(values) }`, 13},
		{"slice assignment copies header", `func Test() int { values := []int{2, 3, 5}; old := values; values = values[:1]; return len(old)*10 + len(values) }`, 31},
		{"assign nil slice", `func Test() int { values := []int{2, 3, 5}; values = nil; return len(values)*10 + cap(values) }`, 0},
		{"compare nil slice", `func Test() int { var values []int; if values == nil { return 42 }; return 0 }`, 42},
		{"compare nil slice struct field", `type holder struct { values []int }; func Test() int { var value holder; if value.values == nil { return 42 }; return 0 }`, 42},
		{"pointer array struct field", `type pair struct { left, right int }; func Test() int { var values [2]pair; values[1].left = 7; values[1].right = 11; pointer := &values; return pointer[1].left*10 + pointer[1].right }`, 81},
		{"struct assignment copies value and preserves address", `type pair struct { left, right int }; func Test() int {
			source := pair{left: 7, right: 11}
			copy := source
			pointer := &copy
			source.left = 17
			if copy.left != 7 { return 0 }
			copy = source
			return pointer.left + pointer.right
		}`, 28},
		{"parallel struct assignment snapshots values", `type pair struct { left, right int }; func Test() int {
			left := pair{left: 1, right: 2}
			right := pair{left: 3, right: 4}
			left, right = right, left
			return left.left*1000 + left.right*100 + right.left*10 + right.right
		}`, 3412},
		{"struct equality compares fields", `type mask struct { count int32; data *byte }; func Test() int {
			var left mask
			var right mask
			if left != right { return 0 }
			right.count = 7
			if left == right { return 0 }
			right.count = 0
			value := byte(1)
			right.data = &value
			if left == right { return 0 }
			return 42
		}`, 42},
		{"range array of structs", `type pair struct { left, right int }; func Test() int { var values [2]pair; values[0].left = 3; values[0].right = 5; values[1].left = 7; values[1].right = 11; total := 0; for _, value := range values { total += value.left + value.right }; return total }`, 26},
		{"returned array survives callee frame", `func makeValues() [3]int { return [3]int{7, 11, 13} }; func disturb() int { values := [3]int{100, 200, 300}; return values[0] }; func Test() int { values := makeValues(); disturb(); return values[0] + values[1] + values[2] }`, 31},
		{"returned struct survives callee frame", `type pair struct { left, right int }; func makePair() pair { return pair{17, 25} }; func disturb() int { value := pair{100, 200}; return value.left }; func Test() int { value := makePair(); disturb(); return value.left + value.right }`, 42},
		{"returned string survives callee frame", `func makeText() string { text := "abc"; return text[:] }; func disturb() int { values := [4]int{100, 200, 300, 400}; return values[0] }; func Test() int { text := makeText(); disturb(); return len(text)*10 + int(text[1]) }`, 128},
		{"zero and copied strings", `func empty() (result string) { return }; func Test() int { var left string; right := left; left = "abc"; return len(empty())*100 + len(right)*10 + len(left) }`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, optimized := range []bool{false, true} {
				name := "unoptimized"
				if optimized {
					name = "optimized"
				}
				t.Run(name, func(t *testing.T) { runCase(t, "package main\n"+tc.body, tc.want, optimized) })
			}
		})
	}
}

func TestAdvancedExecutionCorpus(t *testing.T) {
	cases := []struct {
		name, body string
		want       int
	}{
		{"short circuit and", `var calls int; func yes() bool { calls++; return true }; func Test() int { if false && yes() {}; return calls }`, 0},
		{"short circuit or", `var calls int; func yes() bool { calls++; return true }; func Test() int { if true || yes() {}; return calls }`, 0},
		{"label in eliminated constant branch", `const enabled = true; func Test() int { if enabled { return 42 } else { unused: for { break unused } }; return 0 }`, 42},
		{"switch", `func Test() int { x:=3; switch x { case 1: return 10; case 2,3: return 20; default: return 30 } }`, 20},
		{"switch fallthrough", `func Test() int { n:=0; switch 2 { case 2: n+=2; fallthrough; case 3: n+=3 }; return n }`, 5},
		{"string switch compares contents", `func Test() int { value := ""; switch value { case "on": return 1; case "": return 42; default: return 2 } }`, 42},
		{"package constants", `const base=40; const two int=2; func Test() int { return base+two }`, 42},
		{"dynamic numeric conversions", `func Test() int { value := int64(84); half := float64(value) / 2; narrowed := float32(half); return int(narrowed) }`, 42},
		{"mutable globals", `var total=3; func add(x int){ total+=x }; func Test() int { add(4); add(5); return total }`, 12},
		{"global zero and constant strings", `var empty string; var text = "abc"; func Test() int { return len(empty)*100 + len(text)*10 + int(text[1]) }`, 128},
		{"constant struct global", `type bounds struct { low uint16; high uintptr }; var limits = bounds{low: 7, high: 0x123456789}; func Test() int { return int(limits.low) + int(limits.high&0xff) }`, 144},
		{"global slice assignment", `var values []byte; func set(){ values = []byte{7, 11, 13} }; func Test() int { set(); return len(values)*1000 + cap(values)*100 + int(values[0]+values[1]+values[2]) }`, 3331},
		{"static numeric global slice", `var values = []uint32{7, 11, 24}; func Test() int { return int(values[0] + values[1] + values[2]) }`, 42},
		{"dynamic fields in global composite", `type entry struct { predicate func(int) bool }; func atLeast(limit int) func(int) bool { return func(value int) bool { return value >= limit } }; var entries = []entry{{predicate: atLeast(17)}, {predicate: atLeast(25)}}; func Test() int { if entries[0].predicate(16) || !entries[0].predicate(17) || entries[1].predicate(24) || !entries[1].predicate(25) { return 0 }; return 42 }`, 42},
		{"global composite references variables and function", `type entry struct { apply func(int) int; name string }; var base = 37; var label = "go"; func addBase(value int) int { return base + value }; var current = entry{apply: addBase, name: label}; func Test() int { return current.apply(3) + len(current.name) }`, 42},
		{"escaping aggregate closure capture", `type state struct { value int }; func counter() func() int { current := state{value: 40}; return func() int { current.value++; return current.value } }; func Test() int { next := counter(); _ = next(); return next() }`, 42},
		{"interface string extraction preserves descriptor", `func Test() int { var boxed any = "hello"; asserted, ok := boxed.(string); if !ok { return 0 }; switch value := boxed.(type) { case string: return len(asserted) + len(value) + 32 }; return 0 }`, 42},
		{"dynamic global slice descriptor survives initializer frame", `var values = make([]int, 3); func setup(){ values[0] = 7; values[1] = 11; values[2] = 24 }; func disturb(){ temporary := [8]int{1,2,3,4,5,6,7,8}; _ = temporary }; func Test() int { setup(); disturb(); return values[0] + values[1] + values[2] }`, 42},
		{"global zero slice", `var values []int; func Test() int { return len(values)*10 + cap(values) }`, 0},
		{"global slice backing survives callee", `var values []int; func set(){ values = []int{7, 11, 13} }; func disturb(){ temporary := [4]int{100, 200, 300, 400}; _ = temporary }; func Test() int { set(); disturb(); return values[0] + values[1] + values[2] }`, 31},
		{"slice literal copies struct elements", `type pair struct { left int; right int }; func Test() int { values := []pair{{left: 17, right: 25}}; return values[0].left + values[0].right }`, 42},
		{"map literal copies string keys and struct values", `type record struct { value int }; func Test() int { values := map[string]record{"left": {value: 17}, "right": {value: 25}}; return values["left"].value + values["right"].value }`, 42},
		{"map growth marks inserted slot occupied", `func Test() int {
			values := make(map[int]bool)
			keys := []int{1, 2, 3, 4, 5, 6, 7, 9, 0, 8}
			for _, key := range keys {
				values[key] = true
			}
			if len(values) != len(keys) {
				return 0
			}
			for key := 0; key < len(keys); key++ {
				if !values[key] {
					return 0
				}
			}
			return 42
		}`, 42},
		{"forward multiple results", `func pair() (int, int) { return 17, 25 }; func forward() (int, int) { return pair() }; func Test() int { left, right := forward(); return left + right }`, 42},
		{"multiple aggregate results", `type pair struct { left, right int }; func values() (pair, pair) { return pair{17, 25}, pair{5, 7} }; func Test() int { first, second := values(); return first.left + first.right + second.left - second.right + 2 }`, 42},
		{"nil interface after aggregate result", `type pair struct { left, right int }; func values() (pair, error) { return pair{17, 25}, nil }; func overwrite() [4]int { return [4]int{1, 2, 3, 4} }; func Test() int { value, err := values(); scratch := overwrite(); if err != nil { return -1 }; return value.left + value.right + scratch[3] - 4 }`, 42},
		{"generic globals function values and forwarded results", `func identity[T any](value T) T { return value }; var global = identity(func() int { return 17 }); func retry[T any](fn func() (T, error)) (T, error) { return fn() }; func forward() (int, error) { return retry(func() (int, error) { return 25, nil }) }; func Test() int { selected := identity[int]; first := selected(global()); second, err := forward(); if err != nil { return -1 }; return first + second }`, 42},
		{"named aggregate results", `type pair struct { left, right int }; func values() (first pair, second pair) { first = pair{17, 25}; second = pair{5, 7}; return }; func Test() int { first, second := values(); return first.left + first.right + second.left - second.right + 2 }`, 42},
		{"slice first multi result", `func step(values []byte) ([]byte, bool) { return values[1:], true }; func Test() int { values, ok := step([]byte{7, 11, 13}); values, ok = step(values); if !ok { return 0 }; return len(values)*10 + int(values[0]) }`, 23},
		{"slice multi result loop", `func step(values []byte) ([]byte, bool) { if len(values) == 1 { return nil, false }; return values[1:], true }; func Test() int { values := []byte{7, 11, 13}; count := 0; for { var ok bool; values, ok = step(values); if !ok { break }; count++ }; return count * 10 }`, 20},
		{"nil aggregate result", `func value() (int, error) { return 42, nil }; func Test() int { result, err := value(); if err != nil { return 0 }; return result }`, 42},
		{"named interface result assignment", `func value() (result any) { result = 42; return }; func Test() int { return value().(int) }`, 42},
		{"implicit pointer method receiver", `type counter int; func (value *counter) add(amount int) { *value += counter(amount) }; func Test() int { var value counter = 17; value.add(25); return int(value) }`, 42},
		{"promoted embedded method receiver", `type flags uint8; func (f *flags) set() { *f |= 1 }; type state struct { count uint16; flags }; func Test() int { s := state{count: 510}; s.set(); return int(s.count) + int(s.flags)*1000 }`, 1510},
		{"promoted embedded scalar value receiver", `type flags uintptr; func (f flags) kind() int { return int(f & 31) }; type value struct { flags }; func Test() int { v := value{flags: 42}; return v.kind() }`, 10},
		{"promoted embedded interface method", `type source interface { Value() int }; type implementation struct { value int }; func (value *implementation) Value() int { return value.value }; type wrapper struct { source }; func Test() int { var value source = &wrapper{source: &implementation{value: 42}}; return value.Value() }`, 42},
		{"promoted field through embedded pointer", `type inner struct { value int }; type outer struct { *inner }; func Test() int { value := outer{inner: &inner{value: 42}}; return value.value }`, 42},
		{"global elided pointer struct slice", `type item struct { value int }; var items = []*item{{value: 17}, {value: 25}}; func Test() int { return items[0].value + items[1].value }`, 42},
		{"global concrete error interface", `type textError string; func (value textError) Error() string { return string(value) }; var failure error = textError("bad"); func Test() int { if failure != nil { return 42 }; return 0 }`, 42},
		{"global composite interface initializer", `type source interface { Value() int }; type implementation struct { value int }; func (value implementation) Value() int { return value.value }; var global source = implementation{value: 42}; func Test() int { return global.Value() }`, 42},
		{"global string slice inline headers", `var values = []string{0: "a", 1: "forty-two", 2: "z"}; func Test() int { if values[1] != "forty-two" { return 0 }; return len(values[0])*40 + len(values[2])*2 }`, 42},
		{"global float slice data", `var values = []float64{0: -3.25, 1: 42.5, 2: 1000.75}; func Test() int { if values[0] != -3.25 || values[2] != 1000.75 { return 0 }; return int(values[1]) }`, 42},
		{"global struct nil interface field", `var values = []struct { failure error; value int }{{failure: nil, value: 42}}; func Test() int { if values[0].failure != nil { return 0 }; return values[0].value }`, 42},
		{"global static struct interface field", `type source interface {
			Value() int
		}

		type emptySource struct{}

		func (emptySource) Value() int {
			return 42
		}

		type holder struct {
			source source
		}

		var global = &holder{source: emptySource{}}

		func Test() int {
			return global.source.Value()
		}`, 42},
		{"type switch interface case", `type textError string

		func (value textError) Error() string {
			return string(value)
		}

		func Test() int {
			var value any = textError("forty-two")
			switch converted := value.(type) {
			case error:
				if converted.Error() == "forty-two" {
					return 42
				}
			}
			return 0
		}`, 42},
		{"concrete interface assertion", `type item struct { value int }; func Test() int { var value any = &item{value: 42}; return value.(*item).value }`, 42},
		{"direct interface assertion", `type source interface { Value() int }; type extended interface { source; Extra() int }; type implementation struct{}; func (*implementation) Value() int { return 17 }; func (*implementation) Extra() int { return 25 }; func Test() int { var value source = &implementation{}; converted := value.(extended); return converted.Value() + converted.Extra() }`, 42},
		{"pointer to slice local through interface", `func value() any { values := make([]int, 35); values[34] = 7; return &values }; func Test() int { values := value().(*[]int); return len(*values) + (*values)[34] }`, 42},
		{"interface struct field assignment", `type item struct { value int }; type holder struct { value any }; func set(target *holder, value any) { target.value = value }; func Test() int { var target holder; set(&target, &item{value: 42}); return target.value.(*item).value }`, 42},
		{"interface struct field composite", `type item struct { value int }; type holder struct { value any }; func Test() int { target := holder{value: &item{value: 42}}; return target.value.(*item).value }`, 42},
		{"interface method in composite field", `type source interface {
			Value() int
		}

		type implementation struct {
			value int
		}

		func (value *implementation) Value() int {
			return value.value
		}

		type holder struct {
			source source
		}

		func Test() int {
			value := holder{
				source: &implementation{value: 42},
			}
			return value.source.Value()
		}`, 42},
		{"named function interface receiver", `type operation func(int) int

		func (function operation) Apply(value int) int {
			return function(value)
		}

		type applier interface {
			Apply(int) int
		}

		func addTwo(value int) int {
			return value + 2
		}

		func Test() int {
			var function applier = operation(addTwo)
			return function.Apply(40)
		}`, 42},
		{"method value forwards multiple results", `type pair struct {
			left  int
			right int
		}

		func (value pair) Values() (int, int) {
			return value.left, value.right
		}

		func Test() int {
			values := pair{left: 17, right: 25}.Values
			left, right := values()
			return left + right
		}`, 42},
		{"nil interface struct field", `type item struct { value int }; type holder struct { value any }; func Test() int { target := holder{value: &item{value: 42}}; target.value = nil; if target.value == nil { return 42 }; return 0 }`, 42},
		{"aggregate interface payload survives callee", `
			type boxedValue struct { left int; padding [24]int; right int }
			var boxed any
			func saveBoxed() { boxed = boxedValue{left: 17, right: 25} }
			func disturbBoxedStack() { temporary := [64]int{}; for index := range temporary { temporary[index] = index + 100 }; _ = temporary }
			func Test() int { saveBoxed(); disturbBoxedStack(); value := boxed.(boxedValue); return value.left + value.right }
		`, 42},
		{"returned interface survives callee frame", `
			type counterValue interface { Add(int); Value() int }
			type returnedCounter struct { value int }
			func (counter *returnedCounter) Add(amount int) { counter.value += amount }
			func (counter *returnedCounter) Value() int { return counter.value }
			func makeCounter() counterValue { return &returnedCounter{} }
			func Test() int { counter := makeCounter(); counter.Add(42); return counter.Value() }
		`, 42},
		{"secondary interface result survives callee frame", `
			type counterValue interface { Value() int }
			type returnedCounter struct { value int }
			func (counter *returnedCounter) Value() int { return counter.value }
			func makeCounter() (int, counterValue) { return 17, &returnedCounter{value: 25} }
			func Test() int { first, counter := makeCounter(); return first + counter.Value() }
		`, 42},
		{"global struct slice contains string slice", `type entry struct { name string; values []string }; var entries = []entry{{name: "first", values: []string{"a", "bc"}}, {name: "second", values: []string{"def"}}}; func Test() int { return len(entries[0].name) + len(entries[0].values)*10 + len(entries[0].values[1]) + len(entries[1].values[0]) + len(entries[1].name) }`, 36},
		{"global array of structs", `type pair struct { low byte; high byte }; var pairs = [4]pair{{1, 2}, {3, 4}, 3: {19, 23}}; func Test() int { return int(pairs[0].low + pairs[0].high + pairs[1].low + pairs[1].high + pairs[2].low + pairs[3].low + pairs[3].high) }`, 52},
		{"global struct slice with array field", `type caseRange struct { low uint32; delta [3]int32 }; var ranges = []caseRange{{low: 0x391, delta: [3]int32{0, 32, 0}}}; func Test() int { return int(ranges[0].delta[1]) + 10 }`, 42},
		{"range over temporary slice literal", `func Test() int { total := 0; for _, value := range []int{5, 8, 13, 16} { total += value }; return total }`, 42},
		{"implicit pointer composite literal", `type pair struct { left, right int }; func Test() int { values := []*pair{{left: 17, right: 25}}; return values[0].left + values[0].right }`, 42},
		{"deferred function call", `var result int; func set(value int) { result = value }; func apply() { defer set(42) }; func Test() int { apply(); return result }`, 42},
		{"return expression is evaluated before defer", `var value = 1
			func updateValue() { value = 2 }
			func deferredValue() int { defer updateValue(); return value }
			func Test() int { return deferredValue()*10 + value }`, 12},
		{"defer updates explicit named result", `func deferredResult() (result int) {
			defer func() { result += 2 }()
			return 40
		}
		func Test() int { return deferredResult() }`, 42},
		{"defer updates named result from captured pointer", `type resultState struct {
			code int
		}

		func run() (code int) {
			state := &resultState{code: 42}
			defer func() {
				code = state.code
			}()
			return
		}

		func Test() int {
			return run()
		}`, 42},
		{"recover outside panic", `func Test() int { if recover() == nil { return 42 }; return 0 }`, 42},
		{"append scalar growth", `func Test() int { var values []int; values = append(values, 7); values = append(values, 11, 13); return len(values)*100 + cap(values)*10 + values[0] + values[1] + values[2] }`, 361},
		{"append slice ellipsis", `func Test() int { values := []int{7}; more := []int{11, 13}; values = append(values, more...); return values[0] + values[1] + values[2] }`, 31},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runCase(t, "package main\n"+tc.body, tc.want, false) })
	}
}

func TestRuntimePanicRecover(t *testing.T) {
	runExecutableCase(t, `package main

func trigger() {
	defer func() {
		if recover() == nil {
			panic("missing panic")
		}
	}()
	panic("boom")
}

var normalRecoverResult int

func recoverWithoutPanic() {
	defer func() {
		if recover() == nil {
			normalRecoverResult = 42
		} else {
			normalRecoverResult = 1
		}
	}()
}

func returnedRecoverValue() (value any) {
	defer func() {
		value = recover()
	}()
	return
}

func Test() int {
	trigger()
	if recover() != nil {
		return 1
	}
	recoverWithoutPanic()
	if returnedRecoverValue() != nil {
		return 2
	}
	return normalRecoverResult
}
`, 42)
}

func TestRuntimeDeferReturnValues(t *testing.T) {
	runExecutableCase(t, `package main

var returnOrderValue = 1

func updateReturnOrderValue() {
	returnOrderValue = 2
}

func returnBeforeDefer() int {
	defer updateReturnOrderValue()
	return returnOrderValue
}

func updateFloatResult() (result float64) {
	defer func() {
		result += 2
	}()
	return 40
}

func sourcePair() (int, int) {
	return 16, 24
}

func updateForwardedResults() (left int, right int) {
	defer func() {
		left++
		right++
	}()
	return sourcePair()
}

func Test() int {
	ordered := returnBeforeDefer()*10 + returnOrderValue
	if ordered != 12 {
		return ordered
	}
	if updateFloatResult() != 42 {
		return 1
	}
	left, right := updateForwardedResults()
	return left + right
}
`, 42)
}

func TestStandardLibrarySHA256(t *testing.T) {
	runCase(t, `package main

import "crypto/sha256"

func Test() int {
	sum := sha256.Sum256([]byte("abc"))
	fingerprint := 0
	for i := 0; i < len(sum); i++ {
		fingerprint = (fingerprint*257 + int(sum[i])) % 2147483647
	}
	return fingerprint
}
`, 739054043, false)
}

func TestRepositoryStandardLibraryUTF8(t *testing.T) {
	runCase(t, `package main

import "unicode/utf8"

func Test() int {
	return utf8.RuneLen('世')
}
`, 3, false)
}

func TestUnsafeSliceHeaderDereference(t *testing.T) {
	runCase(t, `package main

import "unsafe"

func Test() int {
	values := [3]int{7, 11, 13}
	header := [3]uintptr{uintptr(unsafe.Pointer(&values[0])), 2, 3}
	slice := *(*[]int)(unsafe.Pointer(&header))
	return len(slice)*100 + cap(slice)*10 + slice[1]
}
`, 241, false)
}

func TestRepositoryStandardLibraryUTF16(t *testing.T) {
	runCase(t, `package main

import "unicode/utf16"

func Test() int {
	return utf16.RuneLen('😀')
}
`, 2, false)
}

func TestRepositoryStandardLibraryUTF16DecodeAllocations(t *testing.T) {
	runExecutableCase(t, `package main

import (
	"testing"
	"unicode/utf16"
)

func Test() int {
	input := []uint16{1, 2, 3, 4}
	allocations := testing.AllocsPerRun(10, func() {
		decoded := utf16.Decode(input)
		if len(decoded) != 4 || decoded[3] != 4 {
			panic("unexpected UTF-16 decode result")
		}
	})
	if allocations != 0 {
		return -1
	}
	return 42
}
`, 42)
}

func TestRuntimeDynamicGlobalInitializerUsesImportedGlobal(t *testing.T) {
	runExecutableCase(t, `package main

import . "path"

var badPattern = ErrBadPattern

func Test() int {
	_, err := Match("[", "value")
	if err != badPattern {
		return -1
	}
	return 42
}
`, 42)
}

func TestRepositoryStandardLibraryHex(t *testing.T) {
	runCase(t, `package main

import "encoding/hex"

func Test() int {
	return hex.EncodedLen(17) + hex.DecodedLen(18)
}
`, 43, false)
}

func TestRepositoryStandardLibraryAdler32(t *testing.T) {
	runCase(t, `package main

import "hash/adler32"

func Test() int {
	return int(adler32.Checksum([]byte("abc")))
}
`, 38600999, false)
}

func TestRepositoryStandardLibraryAdler32WriteString(t *testing.T) {
	runExecutableCase(t, `package main

import (
	"encoding"
	"hash/adler32"
	"io"
)

func Test() int {
	input := "abcde"
	for iteration := 0; iteration < 32; iteration++ {
		hash := adler32.New()
		secondHash := adler32.New()
		if _, err := io.WriteString(hash, input[:len(input)/2]); err != nil {
			return -1
		}
		state, err := hash.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil {
			return -1
		}
		appendedState, err := hash.(encoding.BinaryAppender).AppendBinary(make([]byte, 4, 32))
		if err != nil || string(state) != string(appendedState[4:]) {
			return -1
		}
		if err := secondHash.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
			return -1
		}
		if _, err := io.WriteString(hash, input[len(input)/2:]); err != nil {
			return -1
		}
		if _, err := io.WriteString(secondHash, input[len(input)/2:]); err != nil {
			return -1
		}
		if hash.Sum32() != secondHash.Sum32() {
			return -1
		}
	}
	return int(adler32.Checksum([]byte("abc")))
}
`, 38600999)
}

func TestRepositoryStandardLibraryBinary(t *testing.T) {
	runCase(t, `package main

import "encoding/binary"

func Test() int {
	var buf [10]byte
	n := binary.PutUvarint(buf[:], 300)
	return n + int(buf[0]) + int(buf[1])
}
`, 176, false)
}

func TestRepositoryStandardLibraryMD5(t *testing.T) {
	runCase(t, `package main

import "crypto/md5"

func Test() int {
	sum := md5.Sum([]byte("abc"))
	return int(sum[0]) + int(sum[15])
}
`, 258, false)
}

func TestRepositoryStandardLibrarySHA1(t *testing.T) {
	runCase(t, `package main

import "crypto/sha1"

func Test() int {
	sum := sha1.Sum([]byte("abc"))
	return int(sum[0]) + int(sum[19])
}
`, 326, false)
}

func TestRepositoryStandardLibraryFNVConstructor(t *testing.T) {
	runCase(t, `package main

import "hash/fnv"

func Test() int {
	fnv.New32a()
	return 1
}
`, 1, false)
}

func TestRuntimeSliceValueSemanticsAcrossABI(t *testing.T) {
	runExecutableCase(t, `package main

func shorten(values []int) {
	values = values[:1]
}

func shortenPointer(values *[]int) {
	*values = (*values)[:1]
}

func stackPassed(
	a0, a1, a2, a3, a4, a5, a6 int,
	a7, a8, a9, a10, a11, a12, a13 int,
	values []int,
) int {
	values = values[:1]
	return len(values)*10 + cap(values)
}

func tail(values []int) []int {
	return values[1:]
}

func Test() int {
	values := []int{7, 11, 13}
	shorten(values)
	if len(values) != 3 || cap(values) != 3 {
		return 1
	}
	if stackPassed(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, values) != 13 {
		return 2
	}
	if len(values) != 3 {
		return 3
	}
	copy := values
	shortenPointer(&copy)
	if len(copy) != 1 || len(values) != 3 {
		return 4
	}
	result := tail(values)
	if len(result) != 2 || cap(result) != 2 || result[0] != 11 {
		return 5
	}
	return 42
}
`, 42)
}

func TestRuntimeSliceValuesThroughInterfaceMethod(t *testing.T) {
	runExecutableCase(t, `package main

type transformer interface {
	tail([]int) []int
}

type implementation struct{}

func (implementation) tail(values []int) []int {
	values = values[1:]
	return values
}

func throughInterface(value transformer, values []int) []int {
	return value.tail(values)
}

func Test() int {
	values := []int{7, 11, 13}
	result := throughInterface(implementation{}, values)
	if len(values) != 3 || cap(values) != 3 {
		return 1
	}
	if len(result) != 2 || cap(result) != 2 || result[0] != 11 {
		return 2
	}
	return 42
}
`, 42)
}

func TestRuntimeSliceValuesThroughMethodValue(t *testing.T) {
	runExecutableCase(t, `package main

type cutter struct {
	count int
}

func (value cutter) cut(values []int) []int {
	values = values[value.count:]
	return values
}

func Test() int {
	values := []int{7, 11, 13}
	cut := cutter{count: 1}.cut
	result := cut(values)
	if len(values) != 3 || cap(values) != 3 {
		return 1
	}
	if len(result) != 2 || cap(result) != 2 || result[0] != 11 {
		return 2
	}
	return 42
}
`, 42)
}

func TestRuntimeSliceValuePassedToGoroutine(t *testing.T) {
	runExecutableCase(t, `package main

func shorten(values []int, done chan int) {
	values = values[:1]
	done <- len(values)*10 + cap(values)
}

func Test() int {
	values := []int{7, 11, 13}
	done := make(chan int, 1)
	go shorten(values, done)
	result := <-done
	if result != 13 {
		return 1
	}
	if len(values) != 3 || cap(values) != 3 {
		return 2
	}
	return 42
}
`, 42)
}

func TestRuntimeClosurePassedToGoroutine(t *testing.T) {
	runExecutableCase(t, `package main

func Test() int {
	done := make(chan int, 1)
	value := 40
	go func() {
		done <- value + 2
	}()
	return <-done
}
`, 42)
}

func TestRuntimeClosureWithArgumentsPassedToGoroutine(t *testing.T) {
	runExecutableCase(t, `package main

func Test() int {
	done := make(chan int, 1)
	go func(value int) {
		done <- value + 2
	}(40)
	return <-done
}
`, 42)
}

func TestRuntimeNestedDeferInDeferredClosure(t *testing.T) {
	runExecutableCase(t, `package main

func worker(done chan int) {
	defer func() {
		defer func() {
			done <- 42
		}()
	}()
}

func Test() int {
	done := make(chan int, 1)
	go worker(done)
	return <-done
}
`, 42)
}

func TestRepositoryStandardLibraryOnceFuncSharesCapture(t *testing.T) {
	runExecutableCase(t, `package main

import "sync"

func Test() int {
	value := 0
	run := sync.OnceFunc(func() {
		value = 42
	})
	run()
	run()
	return value
}
`, 42)
}

func TestRuntimeSelectSendDefault(t *testing.T) {
	runExecutableCase(t, `package main

func Test() int {
	values := make(chan int, 1)
	select {
	case values <- 42:
		return <-values
	default:
		return 1
	}
}
`, 42)
}

func TestRuntimeSelectChoosesDefault(t *testing.T) {
	runExecutableCase(t, `package main

func Test() int {
	values := make(chan int)
	select {
	case values <- 1:
		return 1
	default:
		return 42
	}
}
`, 42)
}

func TestRuntimeSelectReceiveDefault(t *testing.T) {
	runExecutableCase(t, `package main

func Test() int {
	values := make(chan int, 1)
	values <- 42
	select {
	case value, ok := <-values:
		if !ok {
			return 1
		}
		return value
	default:
		return 2
	}
}
`, 42)
}

func TestRuntimeSelectReceiveChoosesDefault(t *testing.T) {
	runExecutableCase(t, `package main

func Test() int {
	values := make(chan int)
	select {
	case <-values:
		return 1
	default:
		return 42
	}
}
`, 42)
}

func TestRuntimeSelectMultipleChannels(t *testing.T) {
	runExecutableCase(t, `package main

func Test() int {
	first := make(chan int, 1)
	second := make(chan int, 1)
	second <- 42
	select {
	case <-first:
		return 1
	case value := <-second:
		return value
	}
}
`, 42)
}

func TestRuntimeSliceToArrayConversionCopiesValue(t *testing.T) {
	runExecutableCase(t, `package main

func Test() int {
	values := []int{4, 8, 15, 2}
	converted := [4]int(values)
	values[0] = 99
	return converted[0]*10 + converted[3]
}
`, 42)
}

func TestRuntimeIntegerToStringConversion(t *testing.T) {
	runExecutableCase(t, `package main

func Test() int {
	ascii := string(rune('A'))
	multibyte := string(rune('世'))
	invalid := string(int64(0x110000))
	if ascii != "A" {
		return 1
	}
	if multibyte != "世" {
		return 2
	}
	if invalid != "�" {
		return 3
	}
	return 42
}
`, 42)
}

func TestRuntimeAppendStringEllipsis(t *testing.T) {
	runExecutableCase(t, `package main

func Test() int {
	bytes := []byte{}
	for _, value := range "[世" {
		bytes = append(bytes, string(value)...)
	}
	if string(bytes) != "[世" {
		return 1
	}
	return 42
}
`, 42)
}

func TestRuntimeAppendMakeToNilSlice(t *testing.T) {
	runExecutableCase(t, `package main

func Test() int {
	source := make([]byte, 42)
	values := append([]byte(nil), make([]byte, 1024)...)
	length := copy(values, source)
	if length != 42 || len(values) != 1024 || cap(values) < 1024 {
		return 1
	}
	return 42
}
`, 42)
}

func TestRuntimeReturnedStringStoredInSlice(t *testing.T) {
	runExecutableCase(t, `package main

import "regexp"

func rewrite(text string) string {
	bytes := []byte{}
	for _, value := range text {
		bytes = append(bytes, string(value)...)
	}
	return string(bytes)
}

func Test() int {
	rewritten := rewrite("[")
	if len(rewritten) != 1 {
		return 10 + len(rewritten)
	}
	if rewritten[0] != '[' {
		return 100 + int(rewritten[0])
	}
	patterns := []string{rewritten}
	for index, pattern := range patterns {
		patterns[index] = rewrite(pattern)
	}
	if patterns[0] != "[" {
		return 3
	}
	matched, err := regexp.MatchString(patterns[0], "value")
	if matched || err == nil {
		return 1
	}
	return 42
}
`, 42)
}

func TestRuntimeStaticGlobalSpecialFloatArray(t *testing.T) {
	runExecutableCase(t, `package main

import "math"

var values = [...]float64{17, math.Inf(1), math.NaN()}

func Test() int {
	if values[0] != 17 || !math.IsInf(values[1], 1) || !math.IsNaN(values[2]) {
		return 1
	}
	return 42
}
`, 42)
}

func TestRepositoryStandardLibraryRegexpMatchString(t *testing.T) {
	runExecutableCase(t, `package main

import "regexp"

func Test() int {
	matched, err := regexp.MatchString("[", "value")
	if matched || err == nil {
		return 1
	}
	matched, err = regexp.MatchString("^val", "value")
	if !matched || err != nil {
		return 2
	}
	return 42
}
`, 42)
}

func TestRepositoryStandardLibraryRegexpErrorMethod(t *testing.T) {
	runExecutableCase(t, `package main

import (
	"fmt"
	"regexp"
)

func checkError(values ...any) bool {
	value := values[0]
	converted, ok := value.(error)
	return ok && len(converted.Error()) != 0
}

func Test() int {
	_, err := regexp.MatchString("[", "value")
	if err == nil {
		return 1
	}
	message := err.Error()
	if len(message) == 0 {
		return 2
	}
	var value any = err
	converted, ok := value.(error)
	if !ok || len(converted.Error()) == 0 {
		return 3
	}
	if !checkError(err) {
		return 4
	}
	formatted := fmt.Sprintf("%s", err)
	if len(formatted) == 0 {
		return 5
	}
	return 42
}
`, 42)
}

func TestRuntimeMultiValueCallExpandsAsArguments(t *testing.T) {
	runExecutableCase(t, `package main

func values() (string, int) {
	return "", 42
}

func consume(text string, value int) int {
	if text != "" {
		return 1
	}
	return value
}

func Test() int {
	return consume(values())
}
`, 42)
}

func TestRuntimeInterfaceToInterfaceAssertionPreservesDescriptor(t *testing.T) {
	runExecutableCase(t, `package main

type value interface {
	Value() int
}

type extendedValue interface {
	value
	Valid() bool
}

type item struct {
	n int
}

func (i *item) Value() int {
	return i.n
}

func (i *item) Valid() bool {
	return true
}

func Test() int {
	var original value = &item{n: 42}
	extended, ok := original.(extendedValue)
	if !ok || !extended.Valid() {
		return 1
	}
	return extended.Value()
}
`, 42)
}

func TestRuntimeRangeOverFunction(t *testing.T) {
	runExecutableCase(t, `package main

func values(yield func(int) bool) {
	for value := 1; value <= 6; value++ {
		if !yield(value) {
			return
		}
	}
}

func Test() int {
	total := 0
	for value := range values {
		if value%2 == 0 {
			continue
		}
		if value == 5 {
			break
		}
		total += value
	}
	return total*10 + 2
}
`, 42)
}

func TestRuntimeNamedInterfaceResultAddress(t *testing.T) {
	runExecutableCase(t, `package main

import "unsafe"

type interfaceHeader struct {
	typ  unsafe.Pointer
	data unsafe.Pointer
}

type item struct {
	value int
}

func copyInterface(input any) (result any) {
	source := (*interfaceHeader)(unsafe.Pointer(&input))
	destination := (*interfaceHeader)(unsafe.Pointer(&result))
	destination.typ = source.typ
	destination.data = source.data
	return
}

func Test() int {
	result := copyInterface(&item{value: 42})
	return result.(*item).value
}
`, 42)
}

func TestRuntimeAtomicValueChannelRoundTrip(t *testing.T) {
	runExecutableCase(t, `package main

import "sync/atomic"

func Test() int {
	var value atomic.Value
	channel := make(chan struct{}, 1)
	value.Store(channel)
	loaded := value.Load().(chan struct{})
	loaded <- struct{}{}
	<-loaded
	return 42
}
`, 42)
}

func TestRuntimeContextCancelDone(t *testing.T) {
	runExecutableCase(t, `package main

import "context"

func Test() int {
	ctx, cancel := context.WithCancel(context.Background())
	done := ctx.Done()
	if done == nil {
		return 1
	}
	cancel()
	<-done
	return 42
}
`, 42)
}

func TestRuntimeStringSliceConversionsCopy(t *testing.T) {
	runExecutableCase(t, `package main

func Test() int {
	bytes := []byte("alpha")
	text := string(bytes)
	bytes[0] = 'z'
	if text != "alpha" {
		return 1
	}

	copy := []byte(text)
	copy[1] = 'z'
	if text != "alpha" || string(copy) != "azpha" {
		return 2
	}

	runes := []rune("世界")
	if string(runes) != "世界" {
		return 3
	}
	return 42
}
`, 42)
}

func TestRuntimeRangeOverUntypedStringConstant(t *testing.T) {
	runExecutableCase(t, `package main

func Test() int {
	const text = "a世"
	total := 0
	for index, value := range text {
		total += index + int(value)
	}
	if total != int('a')+1+int('世') {
		return 1
	}
	return 42
}
`, 42)
}

func TestRepositoryStandardLibraryFileModeString(t *testing.T) {
	runExecutableCase(t, `package main

import "io/fs"

func Test() int {
	if fs.FileMode(0o644).String() != "-rw-r--r--" {
		return 1
	}
	return 42
}
`, 42)
}

func TestRuntimeStackPassedSliceSurvivesGarbageCollection(t *testing.T) {
	runExecutableCase(t, `package main

import "runtime"

type item struct {
	value int
}

func retain(
	a0, a1, a2, a3, a4, a5, a6 int,
	a7, a8, a9, a10, a11, a12, a13 int,
	values []*item,
) int {
	runtime.GC()
	return values[0].value
}

func Test() int {
	values := []*item{&item{value: 42}}
	return retain(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, values)
}
`, 42)
}

func TestRuntimePointerSliceBackingScansElements(t *testing.T) {
	runExecutableCase(t, `package main

import "runtime"

type item struct {
	value int
}

func inspect(values []*item) int {
	runtime.GC()
	return values[0].value
}

func Test() int {
	value := &item{value: 42}
	values := []*item{value}
	return inspect(values)
}
`, 42)
}

func TestRuntimePointerToSliceLocalEscapes(t *testing.T) {
	runExecutableCase(t, `package main

import "runtime"

func makeValues() any {
	values := make([]int, 35)
	values[34] = 7
	return &values
}

func Test() int {
	values := makeValues().(*[]int)
	runtime.GC()
	return len(*values) + (*values)[34]
}
`, 42)
}

func TestRuntimeClosureSurvivesGarbageCollectionInCallee(t *testing.T) {
	runExecutableCase(t, `package main

import "runtime"

func apply(callback func(int) int) int {
	runtime.GC()
	return callback(2)
}

func Test() int {
	total := 0
	for index := 0; index < 100; index++ {
		base := 40 + index
		callback := func(value int) int {
			return base + value
		}
		total += apply(callback) - index
	}
	return total
}
`, 4200)
}

func TestRepositoryStandardLibraryContainerList(t *testing.T) {
	runExecutableCase(t, `package main

import "container/list"

func Test() int {
	values := list.New()
	values.PushBack(11)
	values.PushFront(7)
	values.PushBack(13)
	sum := 0
	for element := values.Front(); element != nil; element = element.Next() {
		sum += element.Value.(int)
	}
	return values.Len()*100 + sum
}
`, 331)
}

func TestRepositoryStandardLibraryContainerRing(t *testing.T) {
	runExecutableCase(t, `package main

import "container/ring"

func Test() int {
	values := ring.New(3)
	values.Value = 7
	values = values.Next()
	values.Value = 11
	values = values.Next()
	values.Value = 13
	sum := 0
	values.Do(func(value any) {
		sum += value.(int)
	})
	return values.Len()*100 + sum
}
`, 331)
}

func TestRepositoryStandardLibrarySort(t *testing.T) {
	runExecutableCase(t, `package main

import "sort"

func Test() int {
	values := []int{9, -4, 5, 3}
	sort.Ints(values)
	if values[0] != -4 {
		return 1000 + values[0]
	}
	if values[1] != 3 {
		return 2000 + values[1]
	}
	if values[2] != 5 {
		return 3000 + values[2]
	}
	if values[3] != 9 {
		return 4000 + values[3]
	}
	index := sort.SearchInts(values, 5)
	if index != 2 {
		return 5000 + index
	}
	return 42
}
`, 42)
}

func TestRepositoryStandardLibrarySortFindExhaustive(t *testing.T) {
	runExecutableCase(t, `package main

import "sort"

func Test() int {
	for size := 0; size <= 100; size++ {
		for target := 1; target <= size*2+1; target++ {
			compare := func(index int) int {
				return target - (index+1)*2
			}
			position, found := sort.Find(size, compare)
			wantPosition := target / 2
			wantFound := false
			if target%2 == 0 {
				wantPosition--
				wantFound = true
			}
			if position != wantPosition || found != wantFound {
				return 1
			}
		}
	}
	return 42
}
`, 42)
}

func TestRepositoryStandardLibraryContainerHeap(t *testing.T) {
	runExecutableCase(t, `package main

import "container/heap"

type intHeap []int

func (values intHeap) Len() int {
	return len(values)
}

func (values intHeap) Less(left, right int) bool {
	return values[left] < values[right]
}

func (values intHeap) Swap(left, right int) {
	values[left], values[right] = values[right], values[left]
}

func (values *intHeap) Push(value any) {
	*values = append(*values, value.(int))
}

func (values *intHeap) Pop() any {
	old := *values
	last := len(old) - 1
	value := old[last]
	*values = old[:last]
	return value
}

func Test() int {
	values := intHeap{9, 1, 5}
	heap.Init(&values)
	heap.Push(&values, 3)
	first := heap.Pop(&values).(int)
	second := heap.Pop(&values).(int)
	return first*100 + second*10 + len(values)
}
`, 132)
}

func TestRepositoryStandardLibraryMaps(t *testing.T) {
	runExecutableCase(t, `package main

import "maps"

func Test() int {
	original := map[string]int{"alpha": 1, "beta": 2, "gamma": 3}
	clone := maps.Clone(original)
	if !maps.Equal(original, clone) {
		return -1
	}
	maps.DeleteFunc(clone, func(_ string, value int) bool {
		return value%2 == 0
	})
	return len(clone)*100 + clone["gamma"]
}
`, 203)
}

func TestRepositoryStandardLibraryBufio(t *testing.T) {
	runExecutableCase(t, `package main

import (
	"bufio"
	"io"
	"strings"
)

func Test() int {
	reader := bufio.NewReader(strings.NewReader("alpha,beta"))
	first, err := reader.ReadString(',')
	if err != nil {
		return -1
	}
	second, err := reader.ReadString('a')
	if err != nil {
		return -2
	}
	third, err := reader.ReadString('!')
	if err != io.EOF {
		return -3
	}
	return len(first)*10 + len(second) + len(third)
}
`, 64)
}

func TestRepositoryStandardLibraryASCII85(t *testing.T) {
	runExecutableCase(t, `package main

import "encoding/ascii85"

func Test() int {
	source := []byte("easy")
	encoded := make([]byte, ascii85.MaxEncodedLen(len(source)))
	encodedLength := ascii85.Encode(encoded, source)
	decoded := make([]byte, len(source))
	decodedLength, _, err := ascii85.Decode(decoded, encoded[:encodedLength], true)
	if err != nil {
		return -1
	}
	sum := 0
	for _, value := range decoded[:decodedLength] {
		sum += int(value)
	}
	return encodedLength*100 + decodedLength*10 + sum
}
`, 974)
}

func TestRepositoryStandardLibraryBase32(t *testing.T) {
	runExecutableCase(t, `package main

import "encoding/base32"

func Test() int {
	source := []byte("foo")
	encoded := make([]byte, base32.StdEncoding.EncodedLen(len(source)))
	base32.StdEncoding.Encode(encoded, source)
	decoded := make([]byte, base32.StdEncoding.DecodedLen(len(encoded)))
	decodedLength, err := base32.StdEncoding.Decode(decoded, encoded)
	if err != nil {
		return -1
	}
	sum := 0
	for _, value := range decoded[:decodedLength] {
		sum += int(value)
	}
	return len(encoded)*100 + decodedLength*10 + sum
}
`, 1154)
}

func TestRepositoryStandardLibraryBase64(t *testing.T) {
	runExecutableCase(t, `package main

import "encoding/base64"

func Test() int {
	source := []byte("hello")
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(source)))
	base64.StdEncoding.Encode(encoded, source)
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
	decodedLength, err := base64.StdEncoding.Decode(decoded, encoded)
	if err != nil {
		return -1
	}
	sum := 0
	for _, value := range decoded[:decodedLength] {
		sum += int(value)
	}
	return len(encoded)*100 + decodedLength*10 + sum
}
`, 1382)
}

func TestRepositoryStandardLibraryCSV(t *testing.T) {
	runExecutableCase(t, `package main

import (
	"encoding/csv"
	"strings"
)

func Test() int {
	reader := csv.NewReader(strings.NewReader("name,value\nalpha,7\nbeta,11\n"))
	records, err := reader.ReadAll()
	if err != nil {
		return -1
	}
	return len(records)*100 + len(records[1][0])*10 + len(records[2][1])
}
`, 352)
}

func TestRepositoryStandardLibraryIOCopyN(t *testing.T) {
	runExecutableCase(t, `package main

import (
	"bytes"
	"io"
)

func Test() int {
	reader := bytes.NewReader([]byte("abc"))
	written, err := io.CopyN(io.Discard, reader, 1)
	if err != nil || written != 1 {
		return -1
	}
	return reader.Len() * 20 + int(written) * 2
}
`, 42)
}

func TestRuntimeGlobalStructSliceInitializer(t *testing.T) {
	runExecutableCase(t, `package main

type entry struct {
	name  string
	value int
}

var entries = []entry{
	{name: "alpha", value: 17},
	{name: "beta", value: 25},
}

func Test() int {
	if entries[0].name != "alpha" || entries[1].name != "beta" {
		return -1
	}
	return entries[0].value + entries[1].value
}
`, 42)
}

func TestMixedInterfaceConcreteEqualityExecution(t *testing.T) {
	runExecutableCase(t, `package main

type errorCode uintptr

func (code errorCode) Error() string {
	return "error"
}

func failure() error {
	return errorCode(4)
}

func Test() int {
	err := failure()
	if err != errorCode(4) {
		return -1
	}
	if err == errorCode(5) {
		return -2
	}
	return 42
}
`, 42)
}

func TestInterfaceSliceElementConcreteEqualityExecution(t *testing.T) {
	runExecutableCase(t, `package main

func Test() int {
	values := []any{1}
	value := 1
	if value != values[0] {
		return -1
	}
	if value == any(2) {
		return -2
	}
	for _, element := range values {
		if element != value {
			return -3
		}
	}
	return 42
}
`, 42)
}

func TestRepositoryStandardLibraryRuntimeNumCPU(t *testing.T) {
	runCase(t, `package main

import "runtime"

func Test() int {
	return runtime.NumCPU()
}
`, 0, false)
}

func TestMapExecution(t *testing.T) {
	runCase(t, `package main

func Test() int {
	var empty map[int]int
	_, emptyOK := empty[1]
	if emptyOK || len(empty) != 0 {
		return -1
	}
	values := make(map[int]int, 2)
	for i := 0; i < 40; i++ {
		values[i] = i * 2
	}
	delete(values, 7)
	values[3] = 100
	present, ok := values[3]
	last, lastOK := values[39]
	missing, missingOK := values[70]
	if !ok || missingOK {
		return -1
	}
	if !lastOK {
		return -2
	}
	cleared := make(map[int]int)
	cleared[1] = 9
	clear(cleared)
	_, clearedOK := cleared[1]
	if clearedOK || len(cleared) != 0 {
		return -3
	}
	clear(empty)
	return present + last + missing + len(values)
}
`, 217, false)
}

func TestTypeSwitchExecution(t *testing.T) {
	runCase(t, `package main

func classify(value any) int {
	switch value := value.(type) {
	case nil:
		return 1
	case int:
		return value + 10
	case bool:
		if value {
			return 30
		}
		return 31
	default:
		return 40
	}
}

func Test() int {
	return classify(nil) + classify(7) + classify(true)
}
`, 48, false)
}

func TestCapturedClosureExecution(t *testing.T) {
	runCase(t, `package main

func Test() int {
	base := 7
	add := func(value int) int {
		return base + value
	}
	base = 10
	return add(32)
}
`, 42, false)
}

func TestGenericFunctionLiteralParameterExecution(t *testing.T) {
	runCase(t, `package main

func apply[T ~int](value T) int {
	add := func(increment T) int {
		return int(value + increment)
	}
	return add(2)
}

func Test() int {
	return apply(40)
}
`, 42, false)
}

func runCase(t *testing.T, src string, want int, optimized bool) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("ELF execution test")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("system C linker unavailable")
	}
	m, err := goc.Compile("case.go", []byte(src))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if optimized {
		opt.OptimizeModule(m)
	}
	b, err := nativeObject(m)
	if err != nil {
		t.Fatalf("machine code: %v", err)
	}
	d := t.TempDir()
	obj := filepath.Join(d, "case.o")
	if err := os.WriteFile(obj, b, 0o644); err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(d, "harness.c")
	csrc := fmt.Sprintf("#include <stdio.h>\nextern long main_Test(void); int main(void) { long got=main_Test(); if (got != %d) fprintf(stderr, \"got %%ld\\n\", got); return got == %d ? 0 : 1; }\n", want, want)
	if err := os.WriteFile(harness, []byte(csrc), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(d, "case")
	cmd := exec.Command(cc, "-o", exe, harness, obj)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("link: %v\n%s", err, out)
	}
	if out, err := exec.Command(exe).CombinedOutput(); err != nil {
		t.Fatalf("result != %d: %v\n%s", want, err, out)
	}
}

func runExecutableCase(t *testing.T, source string, want int) {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		t.Skip("Go runtime execution test requires Linux ARM64")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("system C linker unavailable")
	}

	program := source + fmt.Sprintf(`
func main() {
	got := Test()
	if got != %d {
		println("got", got, "want", %d)
		panic("standard library result mismatch")
	}
}
`, want, want)
	module, err := goc.CompileExecutable("case.go", []byte(program))
	if err != nil {
		t.Fatalf("compile executable: %v", err)
	}
	objectData, assemblySource, err := arm64.CompileObjectAndAssembly(module)
	if err != nil {
		t.Fatalf("machine code: %v", err)
	}

	directory := t.TempDir()
	objectPath := filepath.Join(directory, "case.o")
	if err := os.WriteFile(objectPath, objectData, 0o644); err != nil {
		t.Fatal(err)
	}
	assemblyPath := filepath.Join(directory, "runtime.S")
	if err := os.WriteFile(assemblyPath, []byte(assemblySource), 0o644); err != nil {
		t.Fatal(err)
	}
	assemblyObjectPath := filepath.Join(directory, "runtime.o")
	assemble := exec.Command(cc, "-c", "-o", assemblyObjectPath, assemblyPath)
	if output, err := assemble.CombinedOutput(); err != nil {
		t.Fatalf("assemble runtime: %v\n%s", err, output)
	}
	executable := filepath.Join(directory, "case")
	link := exec.Command(cc, "-no-pie", "-o", executable, objectPath, assemblyObjectPath)
	if output, err := link.CombinedOutput(); err != nil {
		t.Fatalf("link executable: %v\n%s", err, output)
	}
	if output, err := exec.Command(executable).CombinedOutput(); err != nil {
		t.Fatalf("result != %d: %v\n%s", want, err, output)
	}
}

func nativeObject(m *ir.Module) ([]byte, error) {
	switch runtime.GOARCH {
	case "amd64":
		return amd64.CompileObject(m)
	case "arm64":
		return arm64.CompileObject(m)
	default:
		return nil, fmt.Errorf("unsupported architecture %s", runtime.GOARCH)
	}
}
