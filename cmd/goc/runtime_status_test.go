package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type runtimeCapability struct {
	category    string
	name        string
	source      string
	expectation runtimeCapabilityExpectation
	timeout     time.Duration
	note        string
}

type runtimeCapabilityExpectation int

const (
	runtimeCapabilityMustPass runtimeCapabilityExpectation = iota
	runtimeCapabilityKnownGap
)

func TestARM64RuntimeCapabilityStatus(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("AArch64 Go runtime capability status")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc unavailable")
	}

	directory := t.TempDir()
	compiler := buildGOCForRuntimeCapabilityStatus(t, directory)

	capabilities := []runtimeCapability{
		{
			category:    "core-types",
			name:        "maps-slices-interfaces",
			source:      "runtime_core_types.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "map-growth-gc",
			source:      "runtime_map_growth_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "map-delete-iterate-gc",
			source:      "runtime_map_delete_iter_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "slice-pointer-append-gc",
			source:      "runtime_slice_pointer_append_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "map-interface-keys-gc",
			source:      "runtime_map_interface_keys_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "heap-struct-graph",
			source:      "gc_struct.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "scalar-stack-slots",
			source:      "runtime_accurate_gc_scalars.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stack",
			name:        "growth-preserves-pointers",
			source:      "runtime_stack_growth.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stack",
			name:        "interface-preserved-across-gc",
			source:      "runtime_interface_stack_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "channel-handoff",
			source:      "runtime_goroutine_channel.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "channel-close-range",
			source:      "runtime_channel_close_range.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "select-channels",
			source:      "runtime_select_channels.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "closed-channel-receive",
			source:      "runtime_closed_channel_receive.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "select-closed-channel",
			source:      "runtime_select_closed_channel.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "unbuffered-pingpong",
			source:      "runtime_unbuffered_pingpong.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "closure-survives-gc",
			source:      "runtime_goroutine_closure_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "many-goroutines-gc",
			source:      "runtime_many_goroutines_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "nil-channel-select",
			source:      "runtime_nil_channel_select.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "closed-channel-send-panic",
			source:      "runtime_closed_channel_send_panic.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "double-close-panic",
			source:      "runtime_double_close_panic.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "interface-channel-gc",
			source:      "runtime_interface_channel_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "lock-osthread",
			source:      "runtime_lock_osthread.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-runtime",
			name:        "context-cancel-channel",
			source:      "context_cancel.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "basic-recover",
			source:      "panic_recover.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "nested-defer",
			source:      "nested_defer.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "typed-recover",
			source:      "runtime_panic_typed_recover.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "goexit-runs-defer",
			source:      "runtime_goexit_defer.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "panic-stack-recover-gc",
			source:      "runtime_panic_stack_recover_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "defer-order-panic",
			source:      "runtime_defer_order_panic.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "goroutine-panic-recover",
			source:      "runtime_goroutine_panic_recover.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "sync-pool-interface-gc",
			source:      "sync_pool_interface.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "sync-waitgroup-mutex-once",
			source:      "runtime_sync_waitgroup_mutex.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "sync-cond-rwmutex",
			source:      "runtime_sync_cond_rwmutex.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "sync-map",
			source:      "runtime_sync_map.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "atomic-counter",
			source:      "runtime_atomic_counter.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "atomic-value",
			source:      "runtime_atomic_value.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "time-timers",
			source:      "runtime_timer_sleep.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "gomaxprocs-memstats",
			source:      "gomaxprocs_memstats.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "time-reset-stop",
			source:      "runtime_timer_reset_stop.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "ticker-stop",
			source:      "runtime_ticker_stop.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "select-timeout",
			source:      "runtime_select_timeout.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "callers-stack",
			source:      "runtime_callers_stack.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "debug-gc-controls",
			source:      "runtime_debug_gc_controls.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-value-call",
			source:      "runtime_reflect_value_call.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-make-values",
			source:      "runtime_reflect_make_values.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "finalizer-basic",
			source:      "runtime_finalizer_basic.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stack",
			name:        "panic-stack-gc",
			source:      "runtime_panic_stack_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
	}

	for _, capability := range capabilities {
		capability := capability
		t.Run(capability.category+"/"+capability.name, func(t *testing.T) {
			result := runRuntimeCapabilityProgram(t, compiler, directory, capability)
			if capability.expectation == runtimeCapabilityMustPass && result.err != nil {
				t.Fatalf("%s should pass: %v\n%s", capability.source, result.err, result.output)
			}
			if capability.expectation == runtimeCapabilityKnownGap && result.err != nil {
				t.Logf("KNOWN GAP %s: %s\n%v\n%s", capability.source, capability.note, result.err, result.output)
				return
			}
			if capability.expectation == runtimeCapabilityKnownGap && result.err == nil {
				t.Logf("KNOWN GAP NOW PASSES %s: %s", capability.source, capability.note)
				return
			}
			t.Logf("PASS %s", capability.source)
		})
	}
}

type runtimeCapabilityResult struct {
	output string
	err    error
}

func buildGOCForRuntimeCapabilityStatus(t *testing.T, directory string) string {
	t.Helper()

	compiler := filepath.Join(directory, "goc")
	build := exec.Command("go", "build", "-o", compiler, ".")
	build.Env = append(os.Environ(), "GOCACHE="+filepath.Join(directory, "cache"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build compiler: %v\n%s", err, output)
	}

	return compiler
}

func runRuntimeCapabilityProgram(t *testing.T, compiler string, directory string, capability runtimeCapability) runtimeCapabilityResult {
	t.Helper()

	sourceName := capability.source
	source := filepath.Join("..", "..", "goc", "testdata", sourceName)
	executable := filepath.Join(directory, strings.TrimSuffix(sourceName, ".go")+".bin")

	compile := exec.Command(compiler, "-o", executable, source)
	if output, err := compile.CombinedOutput(); err != nil {
		return runtimeCapabilityResult{
			output: string(output),
			err:    errors.New("compile failed: " + err.Error()),
		}
	}

	timeout := capability.timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	run := exec.CommandContext(ctx, executable)
	run.Env = runtimeCapabilityExecutionEnv()
	output, err := run.CombinedOutput()
	if ctx.Err() != nil {
		err = ctx.Err()
	}

	return runtimeCapabilityResult{
		output: string(bytes.TrimSpace(output)),
		err:    err,
	}
}

func runtimeCapabilityExecutionEnv() []string {
	environment := os.Environ()
	filtered := make([]string, 0, len(environment)+1)

	for _, entry := range environment {
		if strings.HasPrefix(entry, "HTTP_PROXY=") ||
			strings.HasPrefix(entry, "http_proxy=") ||
			strings.HasPrefix(entry, "HTTPS_PROXY=") ||
			strings.HasPrefix(entry, "https_proxy=") ||
			strings.HasPrefix(entry, "ALL_PROXY=") ||
			strings.HasPrefix(entry, "all_proxy=") ||
			strings.HasPrefix(entry, "REQUEST_METHOD=") ||
			strings.HasPrefix(entry, "GOMAXPROCS=") {
			continue
		}
		filtered = append(filtered, entry)
	}

	filtered = append(filtered, "GOMAXPROCS=1")
	return filtered
}
