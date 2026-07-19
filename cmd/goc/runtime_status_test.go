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
			category:    "core-types",
			name:        "type-assertions-switch",
			source:      "runtime_type_assertions_switch.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "interface-comparable-map",
			source:      "runtime_interface_comparable_map.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "slice-copy-overlap",
			source:      "runtime_slice_copy_overlap.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "string-rune-map",
			source:      "runtime_string_rune_map.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "array-map-key",
			source:      "runtime_array_map_key.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "nil-interface-typed-pointer",
			source:      "runtime_nil_interface_typed_pointer.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "complex-arithmetic",
			source:      "runtime_complex_arithmetic.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "append-growth-pointer-elements",
			source:      "runtime_append_growth_pointer_elements.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "map-clear",
			source:      "runtime_map_clear.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "map-iter-delete-insert",
			source:      "runtime_map_iter_delete_insert.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "interface-to-interface",
			source:      "runtime_interface_to_interface.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "slice-three-index",
			source:      "runtime_slice_three_index.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "copy-string-to-bytes",
			source:      "runtime_copy_string_to_bytes.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "method-expression",
			source:      "runtime_method_expression.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "append-self-overlap",
			source:      "runtime_append_self_overlap.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "map-struct-value-replace",
			source:      "runtime_map_struct_value_replace.go",
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
			category:    "gc",
			name:        "keepalive-finalizer",
			source:      "runtime_keepalive_finalizer.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "new-composite-gc",
			source:      "runtime_new_composite_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "finalizer-stack-growth",
			source:      "runtime_finalizer_stack_growth.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "array-copy-pointer-gc",
			source:      "runtime_array_copy_pointer_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "setfinalizer-clear",
			source:      "runtime_setfinalizer_nil.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "map-pointer-values-gc",
			source:      "runtime_map_pointer_values_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "loop-closure-gc",
			source:      "runtime_loop_closure_gc.go",
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
			category:    "stack",
			name:        "recursive-struct-chain-gc",
			source:      "runtime_recursive_struct_chain_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stack",
			name:        "method-value-gc",
			source:      "runtime_method_value_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stack",
			name:        "interface-method-gc",
			source:      "runtime_interface_method_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stack",
			name:        "defer-closure-stack-gc",
			source:      "runtime_defer_closure_stack_gc.go",
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
			category:    "goroutine",
			name:        "channel-direction",
			source:      "runtime_channel_direction.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "worker-fanin-gc",
			source:      "runtime_worker_fanin_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "channel-struct-pointer-gc",
			source:      "runtime_channel_struct_pointer_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "select-send-default",
			source:      "runtime_select_send_default.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "buffered-channel-fifo",
			source:      "runtime_buffered_channel_fifo.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "channel-of-slices-gc",
			source:      "runtime_channel_of_slices_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "channel-nonblocking-receive",
			source:      "runtime_channel_nonblocking_receive.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "channel-close-buffered-receive",
			source:      "runtime_channel_close_buffered_receive.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "select-buffered-send-receive",
			source:      "runtime_select_buffered_send_receive.go",
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
			category:    "defer-panic",
			name:        "defer-argument-evaluation",
			source:      "runtime_defer_argument_evaluation.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "defer-named-return",
			source:      "runtime_defer_named_return.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "sync-once-panic",
			source:      "runtime_sync_once_panic.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "panic-repanic",
			source:      "runtime_panic_repanic.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "recover-outside-panic",
			source:      "runtime_recover_outside_panic.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "panic-recover-identity",
			source:      "runtime_panic_recover_identity.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "defer-replace-panic",
			source:      "runtime_defer_replace_panic.go",
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
			name:        "reflect-map-slice",
			source:      "runtime_reflect_map_slice.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-select",
			source:      "runtime_reflect_select.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-interface-method",
			source:      "runtime_reflect_interface_method.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-deep-equal",
			source:      "runtime_reflect_deep_equal.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-type-metadata",
			source:      "runtime_reflect_type_metadata.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-set-fields",
			source:      "runtime_reflect_set_fields.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-method-metadata",
			source:      "runtime_reflect_method_metadata.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-interface-extract",
			source:      "runtime_reflect_interface_extract.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "finalizer-basic",
			source:      "runtime_finalizer_basic.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "time-afterfunc",
			source:      "runtime_time_afterfunc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "sync-once-value",
			source:      "runtime_sync_once_value.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "time-after",
			source:      "runtime_time_after.go",
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
				t.Logf(
					"KNOWN GAP %s: %s\n%v\n%s",
					capability.source,
					capability.note,
					result.err,
					truncateRuntimeCapabilityOutput(result.output),
				)
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

func truncateRuntimeCapabilityOutput(output string) string {
	const limit = 2000
	if len(output) <= limit {
		return output
	}
	return output[:limit] + "\n... truncated ..."
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
