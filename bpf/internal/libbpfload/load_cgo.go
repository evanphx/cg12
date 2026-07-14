//go:build linux && cgo

// Package libbpfload loads a compiled eBPF ELF through the system libbpf, for
// tests that want the real loader and verifier to judge the emitter's output. It
// dlopen's libbpf at runtime, so there is no build-time dependency and no dev
// headers are needed.
package libbpfload

import "unsafe"

/*
#include <dlfcn.h>
#include <stddef.h>

typedef void* (*om_t)(const void*, size_t, const void*);
typedef int   (*ld_t)(void*);
typedef void  (*cl_t)(void*);

static long cg12_libbpf_load(const void* buf, size_t sz) {
    void* h = dlopen("libbpf.so.1", RTLD_NOW);
    if (!h) return -1000;
    om_t om = (om_t)dlsym(h, "bpf_object__open_mem");
    ld_t ld = (ld_t)dlsym(h, "bpf_object__load");
    cl_t cl = (cl_t)dlsym(h, "bpf_object__close");
    if (!om || !ld) return -1000;
    void* obj = om(buf, sz, NULL);
    long p = (long)obj;
    if (!obj) return -1001;
    if (p < 0 && p > -4096) return p; // libbpf returns an errno-encoded pointer
    int r = ld(obj);
    if (cl) cl(obj);
    return r;
}
*/
import "C"

// Load asks libbpf to open and verify the ELF. It returns 0 on success, a
// negative errno on a load/verify failure (e.g. -1 for EPERM), or -1000 when
// libbpf is unavailable.
func Load(elf []byte) int {
	if len(elf) == 0 {
		return -1001
	}
	return int(C.cg12_libbpf_load(unsafe.Pointer(&elf[0]), C.size_t(len(elf))))
}

// Available reports whether the real libbpf loader is compiled in.
func Available() bool { return true }
