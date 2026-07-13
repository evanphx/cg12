package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// TestStackCopyWalksMultipleFrames proves the multi-frame case a real stack copy
// needs: the runtime walks the frame-pointer chain past an intermediate frame to
// reach a deeper one, uses that frame's suspension PC (its return address, stored
// in the child frame) to look up its stack map, and relocates a root there. The
// call chain is outer -> leaf -> gc_walk; the interior root lives in outer.
func TestStackCopyWalksMultipleFrames(t *testing.T) {
	m := ir.NewModule()

	leaf := m.NewFunc("leaf", ir.ClsW)
	le := leaf.Entry()
	le.CallVoid(leaf.Sym("gc_walk", 0))
	le.Ret(leaf.Word(0))

	outer := m.NewFunc("outer", ir.ClsW).Export()
	seed := outer.Param("seed", ir.ClsW)
	oe := outer.Entry()
	cell := oe.Alloc(4, 4)
	outer.MarkGCRefType(cell, gcStack) // interior stack root, live across the call
	oe.Store(seed, cell)
	oe.CallVoid(outer.Sym("leaf", 0))
	oe.Ret(oe.Load(ir.ClsW, cell))

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)

	code := runObject(t, data, `
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

extern unsigned char __cg12_stackmaps[];
int g_relocated = 0;

// Relocate the interior stack roots of the frame two levels up (outer), reached
// by walking the frame-pointer chain: gc_walk -> leaf -> outer.
void gc_walk(void){
  void *my_fp    = __builtin_frame_address(0);
  void *leaf_fp  = *(void**)my_fp;                        // leaf's frame
  void *outer_fp = *(void**)leaf_fp;                      // outer's frame
  void *outer_pc = *(void**)((char*)leaf_fp + 8);         // return addr into outer

  unsigned char *p = __cg12_stackmaps;
  if (memcmp(p, "SMAP", 4) != 0) return;
  uint32_t count; memcpy(&count, p+8, 4); p += 12;
  for (uint32_t i=0;i<count;i++){
    uint64_t pc; uint32_t nroots;
    memcpy(&pc, p, 8); memcpy(&nroots, p+8, 4); p += 12;
    if ((void*)(uintptr_t)pc == outer_pc){
      for (uint32_t j=0;j<nroots;j++){
        uint8_t kind = p[0];
        int32_t off; uint32_t type;
        memcpy(&off, p+4, 4); memcpy(&type, p+8, 4); p += 12;
        if (kind == 1 && type == 2){                      // frame-slot interior ptr
          int **slot = (int**)((char*)outer_fp + off);
          int *newp = malloc(sizeof(int));
          *newp = **slot + 1000;
          *slot = newp;
          g_relocated++;
        }
      }
      return;
    }
    p += (size_t)nroots * 12;
  }
}

extern int outer(int);
int main(void){
  int r = outer(5);
  if (r != 1005)      return 1;  // outer used the relocated pointer
  if (g_relocated != 1) return 2;
  return 0;
}`)
	require.Equal(t, 0, code)
}

// Type descriptors the proof-of-concept runtime understands.
const (
	gcHeap  = 1 // an ordinary heap reference (do not adjust on a stack move)
	gcStack = 2 // an interior pointer into the stack (adjust on a stack move)
)

// TestStackCopyRelocatesInteriorPointer is the proof of concept for the "can the
// spilled addresses be updated" question. A cg12 function holds two roots across
// a safepoint: a heap pointer and an interior pointer to one of its own stack
// slots. The C "runtime" (gc_move), invoked at the safepoint, uses the typed
// stack map to find the interior pointer in the caller's frame, relocates what it
// points at, and rewrites the spilled pointer — leaving the heap pointer alone.
// The mutator then transparently uses the updated pointer.
func TestStackCopyRelocatesInteriorPointer(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("run", ir.ClsW).Export()
	seed := f.Param("seed", ir.ClsW)
	h := f.ParamRef("h")       // a heap int*
	f.MarkGCRefType(h, gcHeap) // ...typed as heap
	e := f.Entry()
	cell := e.Alloc(4, 4)                                            // an int on this function's stack
	f.MarkGCRefType(cell, gcStack)                                   // its address is an interior stack root
	e.Store(seed, cell)                                              // *cell = seed
	e.CallVoid(f.Sym("gc_move", 0))                                  // safepoint: cell and h are live across it
	e.Ret(e.Add(ir.ClsW, e.Load(ir.ClsW, cell), e.Load(ir.ClsW, h))) // *cell + *h

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)

	code := runObject(t, data, `
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

extern unsigned char __cg12_stackmaps[];
int g_stack_relocated = 0;
int g_heap_seen       = 0;

// A minimal precise "stack mover": at a safepoint, walk to the caller's frame,
// look up its typed roots, and for each interior stack pointer relocate the
// pointee and rewrite the (spilled) pointer. Heap roots are left untouched.
void gc_move(void){
  void *my_fp  = __builtin_frame_address(0);
  void *ret    = __builtin_return_address(0); // safepoint PC in run
  void *run_fp = *(void**)my_fp;              // run's frame pointer (x29)

  unsigned char *p = __cg12_stackmaps;
  if (memcmp(p, "SMAP", 4) != 0) return;
  uint32_t count; memcpy(&count, p+8, 4); p += 12;
  for (uint32_t i=0;i<count;i++){
    uint64_t pc; uint32_t nroots;
    memcpy(&pc, p, 8); memcpy(&nroots, p+8, 4); p += 12;
    if ((void*)(uintptr_t)pc == ret){
      for (uint32_t j=0;j<nroots;j++){
        uint8_t kind = p[0];
        int32_t off; uint32_t type;
        memcpy(&off, p+4, 4); memcpy(&type, p+8, 4); p += 12;
        if (kind != 1) continue;                      // frame slot
        void **slot = (void**)((char*)run_fp + off);  // where the pointer lives
        if (type == 2){                               // interior stack pointer
          int *oldp = *(int**)slot;
          int *newp = malloc(sizeof(int));
          *newp = *oldp + 1000;                       // "relocate" the pointee
          *slot = newp;                               // rewrite the spilled pointer
          g_stack_relocated++;
        } else if (type == 1){
          g_heap_seen++;                              // heap: leave it be
        }
      }
      return;
    }
    p += (size_t)nroots * 12;
  }
}

extern int run(int, int*);
int main(void){
  int hv = 7;
  int r = run(5, &hv);
  // The interior pointer was found via the map and rewritten, so *cell reads the
  // relocated value (5 + 1000); the heap value is untouched.
  if (r != (1005 + 7)) return 1;
  if (g_stack_relocated != 1) return 2;  // exactly one stack root, relocated
  if (g_heap_seen != 1)       return 3;  // exactly one heap root, left alone
  if (hv != 7)                return 4;  // heap object untouched
  return 0;
}`)
	require.Equal(t, 0, code)
}
