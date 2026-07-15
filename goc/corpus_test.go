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
		{"switch", `func Test() int { x:=3; switch x { case 1: return 10; case 2,3: return 20; default: return 30 } }`, 20},
		{"switch fallthrough", `func Test() int { n:=0; switch 2 { case 2: n+=2; fallthrough; case 3: n+=3 }; return n }`, 5},
		{"package constants", `const base=40; const two int=2; func Test() int { return base+two }`, 42},
		{"mutable globals", `var total=3; func add(x int){ total+=x }; func Test() int { add(4); add(5); return total }`, 12},
		{"global zero and constant strings", `var empty string; var text = "abc"; func Test() int { return len(empty)*100 + len(text)*10 + int(text[1]) }`, 128},
		{"constant struct global", `type bounds struct { low uint16; high uintptr }; var limits = bounds{low: 7, high: 0x123456789}; func Test() int { return int(limits.low) + int(limits.high&0xff) }`, 144},
		{"global slice assignment", `var values []byte; func set(){ values = []byte{7, 11, 13} }; func Test() int { set(); return len(values)*1000 + cap(values)*100 + int(values[0]+values[1]+values[2]) }`, 3331},
		{"global zero slice", `var values []int; func Test() int { return len(values)*10 + cap(values) }`, 0},
		{"global slice backing survives callee", `var values []int; func set(){ values = []int{7, 11, 13} }; func disturb(){ temporary := [4]int{100, 200, 300, 400}; _ = temporary }; func Test() int { set(); disturb(); return values[0] + values[1] + values[2] }`, 31},
		{"forward multiple results", `func pair() (int, int) { return 17, 25 }; func forward() (int, int) { return pair() }; func Test() int { left, right := forward(); return left + right }`, 42},
		{"multiple aggregate results", `type pair struct { left, right int }; func values() (pair, pair) { return pair{17, 25}, pair{5, 7} }; func Test() int { first, second := values(); return first.left + first.right + second.left - second.right + 2 }`, 42},
		{"named aggregate results", `type pair struct { left, right int }; func values() (first pair, second pair) { first = pair{17, 25}; second = pair{5, 7}; return }; func Test() int { first, second := values(); return first.left + first.right + second.left - second.right + 2 }`, 42},
		{"slice first multi result", `func step(values []byte) ([]byte, bool) { return values[1:], true }; func Test() int { values, ok := step([]byte{7, 11, 13}); values, ok = step(values); if !ok { return 0 }; return len(values)*10 + int(values[0]) }`, 23},
		{"slice multi result loop", `func step(values []byte) ([]byte, bool) { if len(values) == 1 { return nil, false }; return values[1:], true }; func Test() int { values := []byte{7, 11, 13}; count := 0; for { var ok bool; values, ok = step(values); if !ok { break }; count++ }; return count * 10 }`, 20},
		{"nil aggregate result", `func value() (int, error) { return 42, nil }; func Test() int { result, err := value(); if err != nil { return 0 }; return result }`, 42},
		{"implicit pointer method receiver", `type counter int; func (value *counter) add(amount int) { *value += counter(amount) }; func Test() int { var value counter = 17; value.add(25); return int(value) }`, 42},
		{"promoted embedded method receiver", `type flags uint8; func (f *flags) set() { *f |= 1 }; type state struct { count uint16; flags }; func Test() int { s := state{count: 510}; s.set(); return int(s.count) + int(s.flags)*1000 }`, 1510},
		{"promoted field through embedded pointer", `type inner struct { value int }; type outer struct { *inner }; func Test() int { value := outer{inner: &inner{value: 42}}; return value.value }`, 42},
		{"global elided pointer struct slice", `type item struct { value int }; var items = []*item{{value: 17}, {value: 25}}; func Test() int { return items[0].value + items[1].value }`, 42},
		{"global concrete error interface", `type textError string; func (value textError) Error() string { return string(value) }; var failure error = textError("bad"); func Test() int { if failure != nil { return 42 }; return 0 }`, 42},
		{"concrete interface assertion", `type item struct { value int }; func Test() int { var value any = &item{value: 42}; return value.(*item).value }`, 42},
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
		{"deferred function call", `var result int; func set(value int) { result = value }; func apply() { defer set(42) }; func Test() int { apply(); return result }`, 42},
		{"recover outside panic", `func Test() int { if recover() == nil { return 42 }; return 0 }`, 42},
		{"append scalar growth", `func Test() int { var values []int; values = append(values, 7); values = append(values, 11, 13); return len(values)*100 + cap(values)*10 + values[0] + values[1] + values[2] }`, 361},
		{"append slice ellipsis", `func Test() int { values := []int{7}; more := []int{11, 13}; values = append(values, more...); return values[0] + values[1] + values[2] }`, 31},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runCase(t, "package main\n"+tc.body, tc.want, false) })
	}
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
