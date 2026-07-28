package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

type runtimeCapability struct {
	category       string
	name           string
	source         string
	expectation    runtimeCapabilityExpectation
	timeout        time.Duration
	note           string
	output         string
	requiresAFINET bool
}

type runtimeCapabilityExpectation int

const (
	runtimeCapabilityMustPass runtimeCapabilityExpectation = iota
	runtimeCapabilityKnownGap
	runtimeCapabilityExpectedFailure
)

func TestARM64RuntimeCapabilityStatus(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("AArch64 Go runtime capability status")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc unavailable")
	}
	if *runtimeCoverageRuns < 1 {
		t.Fatalf("-runtime-coverruns must be at least 1")
	}
	if *runtimeCoverageProfile == "" && *runtimeCoverageRuns != 1 {
		t.Fatalf("-runtime-coverruns requires -runtime-coverprofile")
	}
	if *runtimeStatusRuns < 1 {
		t.Fatalf("-runtime-status-runs must be at least 1")
	}
	if *runtimeProcs < 1 {
		t.Fatalf("-runtime-procs must be at least 1")
	}
	if *runtimeStatusShards < 1 {
		t.Fatalf("-runtime-status-shards must be at least 1")
	}
	if *runtimeStatusShard < 0 || *runtimeStatusShard >= *runtimeStatusShards {
		t.Fatalf("-runtime-status-shard must be in [0, %d)", *runtimeStatusShards)
	}

	directory := t.TempDir()
	compiler := buildGOCForRuntimeCapabilityStatus(t, directory)
	afinetSocketAvailable, afinetSocketErr := runtimeCapabilityAFINETSocketAvailable()

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
			name:        "type-param-method-dispatch",
			source:      "runtime_type_param_method_dispatch.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "type-param-method-shapes",
			source:      "runtime_type_param_method_shapes.go",
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
			category:    "core-types",
			name:        "nil-map-read-delete",
			source:      "runtime_nil_map_read_delete.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "map-pointer-keys",
			source:      "runtime_map_pointer_keys.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "interface-nil-assertions",
			source:      "runtime_interface_nil_assertions.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "interface-method-value",
			source:      "runtime_interface_method_value.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "slice-to-array-pointer",
			source:      "runtime_slice_to_array_pointer.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "string-byte-round-trip",
			source:      "runtime_string_byte_round_trip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "map-interface-values-gc",
			source:      "runtime_map_interface_values_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "interface-method-expression",
			source:      "runtime_interface_method_expression.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "type-switch-pointer-values",
			source:      "runtime_type_switch_pointer_values.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "append-variadic-pointer-gc",
			source:      "runtime_append_variadic_pointer_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "copy-interface-slice-gc",
			source:      "runtime_copy_interface_slice_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "map-range-insert-growth",
			source:      "runtime_map_range_insert_growth.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "unsafe-struct-field",
			source:      "runtime_unsafe_struct_field.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "map-range-delete-all",
			source:      "runtime_map_range_delete_all.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "interface-pointer-equality-gc",
			source:      "runtime_interface_pointer_equality_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "core-types",
			name:        "string-slice-compare-swap",
			source:      "runtime_string_slice_compare_swap.go",
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
			name:        "cleanup-basic",
			source:      "runtime_cleanup_basic.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "cleanup-stop",
			source:      "runtime_cleanup_stop.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "cleanup-multiple",
			source:      "runtime_cleanup_multiple.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "cleanup-frame-retention",
			source:      "runtime_cleanup_frame_retention.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "assist-alloc-recursion",
			source:      "runtime_gc_assist_stack_growth.go",
			expectation: runtimeCapabilityKnownGap,
			note:        "minimal reducer for the 5.2.1 GC-assist allocation recursion; concurrent runtime.GC() drives goroutine stacks past 4 MiB within one cycle (see RUNTIME_PLAN.md 5.2.1)",
		},
		{
			category:    "gc",
			name:        "defer-capture-allocs",
			source:      "runtime_defer_capture_allocs.go",
			expectation: runtimeCapabilityKnownGap,
			note:        "compiler-level reducer for 5.2.1: a variable captured by a deferred literal on an untaken branch is still heap-lifted, so runtime.gcAssistAlloc allocates",
		},
		{
			category:    "gc",
			name:        "cleanup-frame-retention-masked",
			source:      "runtime_cleanup_frame_retention_masked.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "cleanup-frame-retention-scribble",
			source:      "runtime_cleanup_frame_retention_scribble.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category: "gc",
			name:     "span-metadata-barrier",
			source:   "runtime_span_metadata_barrier.go",
			// The only capability that raises GOMAXPROCS itself. Its failure
			// mode needs spans grown during marking from the initial thread's
			// g0 stack, which -runtime-procs=1..4 never produces. See 5.2.2.
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "finalizer-cleanup-order",
			source:      "runtime_finalizer_cleanup_order.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "finalizer-dependency-order",
			source:      "runtime_finalizer_dependency_order.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "finalizer-tiny",
			source:      "runtime_finalizer_tiny.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "pinner-lifecycle",
			source:      "runtime_pinner_lifecycle.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "pinner-invalid",
			source:      "runtime_pinner_invalid.go",
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
			category:    "gc",
			name:        "global-roots-gc",
			source:      "runtime_global_roots_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "closure-captures-roots-gc",
			source:      "runtime_closure_captures_roots_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "gc",
			name:        "slice-struct-values-gc",
			source:      "runtime_slice_struct_values_gc.go",
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
			name:        "select-global-locked-gc",
			source:      "runtime_select_global_locked_gc.go",
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
			expectation: runtimeCapabilityKnownGap,
			note:        "performance shortfall from the 5.2.1 GC-assist allocation recursion, not the 5.3 over-retention bug; the live heap stays flat while goroutine stacks and mark time grow superlinearly",
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
			category:    "goroutine",
			name:        "gosched-progress",
			source:      "runtime_gosched_progress.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "select-receive-default",
			source:      "runtime_select_receive_default.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "select-nil-disable",
			source:      "runtime_select_nil_disable.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "channel-interface-close-gc",
			source:      "runtime_channel_interface_close_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "channel-struct-zero-close",
			source:      "runtime_channel_struct_zero_close.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "select-two-ready",
			source:      "runtime_select_two_ready.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "goroutine",
			name:        "channel-pointer-zero-close",
			source:      "runtime_channel_pointer_zero_close.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-runtime",
			name:        "context-cancel-channel",
			source:      "context_cancel.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "scheduler-stress",
			name:        "ring-handoff",
			source:      "runtime_scheduler_ring_handoff.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "scheduler-stress",
			name:        "timer-select-churn",
			source:      "runtime_timer_select_churn.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "scheduler-stress",
			name:        "gc-churn",
			source:      "runtime_scheduler_gc_churn.go",
			expectation: runtimeCapabilityKnownGap,
			note:        "performance shortfall from the 5.2.1 GC-assist allocation recursion, not the 5.3 over-retention bug: the live heap stays flat while goroutine stacks and mark time grow superlinearly, and the program dies with \"goroutine stack exceeds 1000000000-byte limit\" identically before and after the 5.3 fix",
		},
		{
			category:    "stdlib-io",
			name:        "readall-limited-reader",
			source:      "stdlib_io_readall_limited_reader.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "copy-buffer",
			source:      "stdlib_io_copy_buffer.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "multi-reader-writer",
			source:      "stdlib_io_multi_reader_writer.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "tee-reader",
			source:      "stdlib_io_tee_reader.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "pipe-roundtrip",
			source:      "stdlib_io_pipe_roundtrip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "bufio-reader-writer",
			source:      "stdlib_bufio_reader_writer.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "bufio-scanner-words",
			source:      "stdlib_bufio_scanner_words.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "bufio-lines-after-read",
			source:      "bufio_lines_after_read.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "bytes-buffer",
			source:      "stdlib_bytes_buffer.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "strings-reader-seek",
			source:      "stdlib_strings_reader_seek.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "section-reader",
			source:      "stdlib_io_section_reader.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "copy-n",
			source:      "io_copy_n.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "copy-n-writer-only",
			source:      "io_copy_n_writer_only.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "discard-write",
			source:      "io_discard_write.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "limited-reader",
			source:      "io_limited_reader.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "write-string",
			source:      "io_write_string.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-os",
			name:        "file-roundtrip",
			source:      "stdlib_os_file_roundtrip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-os",
			name:        "rename-stat",
			source:      "stdlib_os_rename_stat.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-os",
			name:        "readdir-walkdir",
			source:      "stdlib_os_readdir_walkdir.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-os",
			name:        "pipe-goroutine-close",
			source:      "stdlib_os_pipe_goroutine_close.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-os",
			name:        "pipe-returned-closure-close",
			source:      "stdlib_os_pipe_returned_closure_close.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-os",
			name:        "pipe-interface-capture-close",
			source:      "stdlib_os_pipe_interface_capture_close.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-os",
			name:        "pipe-manual-copy-shape",
			source:      "stdlib_os_pipe_manual_copy_shape.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-os",
			name:        "pipe-embedded-file-copy",
			source:      "stdlib_os_pipe_embedded_file_copy.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-os",
			name:        "exec-goroutine-shape",
			source:      "stdlib_os_exec_goroutine_shape.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-os",
			name:        "filepath-glob-clean",
			source:      "stdlib_filepath_glob_clean.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-os",
			name:        "filemode-string",
			source:      "filemode_string.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-os",
			name:        "user-lookup",
			source:      "stdlib_os_user_lookup.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "base64",
			source:      "stdlib_encoding_base64.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "hex",
			source:      "stdlib_encoding_hex.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "csv",
			source:      "stdlib_encoding_csv.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "binary",
			source:      "stdlib_encoding_binary.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "binary-varint",
			source:      "stdlib_encoding_binary_varint.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "base32",
			source:      "stdlib_encoding_base32.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "ascii85",
			source:      "stdlib_encoding_ascii85.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "pem",
			source:      "stdlib_encoding_pem.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "xml",
			source:      "stdlib_encoding_xml.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "json-roundtrip",
			source:      "stdlib_encoding_json_roundtrip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "gob-int",
			source:      "stdlib_encoding_gob_int.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "gob-struct-int",
			source:      "stdlib_encoding_gob_struct_int.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "gob-struct-mixed",
			source:      "stdlib_encoding_gob_struct_mixed.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "gob-roundtrip",
			source:      "stdlib_encoding_gob_roundtrip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-encoding",
			name:        "asn1-roundtrip",
			source:      "stdlib_encoding_asn1_roundtrip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-crypto",
			name:        "hash-hmac",
			source:      "stdlib_crypto_hash_hmac.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-crypto",
			name:        "aes-modes",
			source:      "stdlib_crypto_aes_modes.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-crypto",
			name:        "hkdf",
			source:      "stdlib_crypto_hkdf.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-crypto",
			name:        "ed25519",
			source:      "stdlib_crypto_ed25519.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-crypto",
			name:        "ecdh-x25519",
			source:      "stdlib_crypto_ecdh_x25519.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-crypto",
			name:        "x509-ed25519",
			source:      "stdlib_crypto_x509_ed25519.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-crypto",
			name:        "rsa",
			source:      "stdlib_crypto_rsa.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-crypto",
			name:        "ecdsa",
			source:      "stdlib_crypto_ecdsa.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-crypto",
			name:        "sha3-pbkdf2",
			source:      "stdlib_crypto_sha3_pbkdf2.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-crypto",
			name:        "des-rc4",
			source:      "stdlib_crypto_des_rc4.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-crypto",
			name:        "mlkem",
			source:      "stdlib_crypto_mlkem.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-crypto",
			name:        "hpke",
			source:      "stdlib_crypto_hpke.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-errors",
			name:        "join-is-as",
			source:      "stdlib_errors_join_is_as.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-text",
			name:        "strconv-roundtrip",
			source:      "stdlib_strconv_roundtrip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-text",
			name:        "strconv-parse-edges",
			source:      "stdlib_strconv_parse_edges.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-text",
			name:        "regexp-find-replace",
			source:      "stdlib_regexp_find_replace.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-text",
			name:        "strings-transform",
			source:      "stdlib_strings_transform.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-text",
			name:        "strings-builder-cut",
			source:      "stdlib_strings_builder_cut.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-text",
			name:        "text-template-parse",
			source:      "stdlib_text_template_parse.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-text",
			name:        "text-template-execute-parse-only",
			source:      "stdlib_text_template_execute_parse_only.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-text",
			name:        "text-template-execute",
			source:      "stdlib_text_template_execute.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-text",
			name:        "sort-search-slice",
			source:      "stdlib_sort_search_slice.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-text",
			name:        "sort-find-edges",
			source:      "stdlib_sort_find_edges.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-fmt",
			name:        "println",
			source:      "fmt_println.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-fmt",
			name:        "sprintf",
			source:      "fmt_sprintf.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-bytes",
			name:        "grow-capacity",
			source:      "bytes_grow_capacity.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-bytes",
			name:        "grow-compare",
			source:      "bytes_grow_compare.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-bytes",
			name:        "grow-stats",
			source:      "bytes_grow_stats.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-bytes",
			name:        "grow-allocs",
			source:      "bytes_grow_allocs.go",
			expectation: runtimeCapabilityKnownGap,
			note:        "performance shortfall from the 5.2.1 GC-assist allocation recursion, not the 5.3 over-retention bug; the recursive assist allocations are also what AllocsPerRun reports as spurious Buffer.Grow allocations",
		},
		{
			category:    "stdlib-bytes",
			name:        "replace-allocs",
			source:      "bytes_replace_allocs.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-bytes",
			name:        "reader-unread",
			source:      "stdlib_bytes_reader_unread.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-containers",
			name:        "list-ring",
			source:      "stdlib_container_list_ring.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-containers",
			name:        "heap",
			source:      "stdlib_container_heap.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-math",
			name:        "math-bits",
			source:      "stdlib_math_bits.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-math",
			name:        "bits-wide-arithmetic",
			source:      "stdlib_math_bits_wide.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-math",
			name:        "math-cmplx",
			source:      "stdlib_math_cmplx.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-generics",
			name:        "maps-slices",
			source:      "stdlib_maps_slices.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-generics",
			name:        "maps-keys",
			source:      "stdlib_maps_keys.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-generics",
			name:        "slices-string-sort",
			source:      "stdlib_slices_string_sort.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-generics",
			name:        "maps-clone",
			source:      "stdlib_maps_clone.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-time",
			name:        "format-parse",
			source:      "stdlib_time_format_parse.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-url",
			name:        "resolve-query",
			source:      "stdlib_url_resolve_query.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-url",
			name:        "values-encode",
			source:      "stdlib_url_values_encode.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-mime",
			name:        "quotedprintable",
			source:      "stdlib_mime_quotedprintable.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-compress",
			name:        "gzip-roundtrip",
			source:      "stdlib_compress_gzip_roundtrip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-compress",
			name:        "zlib-lzw",
			source:      "stdlib_compress_zlib_lzw.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-compress",
			name:        "bzip2",
			source:      "stdlib_compress_bzip2.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-archive",
			name:        "tar-roundtrip",
			source:      "stdlib_archive_tar_roundtrip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-archive",
			name:        "zip-roundtrip",
			source:      "stdlib_archive_zip_roundtrip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-image",
			name:        "png-roundtrip",
			source:      "stdlib_image_png_roundtrip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-image",
			name:        "jpeg-roundtrip",
			source:      "stdlib_image_jpeg_roundtrip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-image",
			name:        "gif-animation",
			source:      "stdlib_image_gif_animation.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-math",
			name:        "rand-shuffle-permutation",
			source:      "stdlib_rand_shuffle_permutation.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-context",
			name:        "cancel-value",
			source:      "stdlib_context_cancel_value.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-flag",
			name:        "parse",
			source:      "stdlib_flag_parse.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-log",
			name:        "buffer",
			source:      "stdlib_log_buffer.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-log",
			name:        "slog-structured",
			source:      "stdlib_slog_structured.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-fs",
			name:        "mapfs-walk",
			source:      "stdlib_fstest_mapfs.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-testing",
			name:        "quick-check",
			source:      "stdlib_testing_quick.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-net-values",
			name:        "mail-textproto",
			source:      "stdlib_net_mail_textproto.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-net-values",
			name:        "netip",
			source:      "stdlib_net_netip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-net-values",
			name:        "smtp-session",
			source:      "stdlib_smtp_session.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-signals",
			name:        "notify-context",
			source:      "stdlib_signal_notify_context.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-signals",
			name:        "repeated-stop-reset",
			source:      "stdlib_signal_repeated_stop_reset.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-signals",
			name:        "during-gc",
			source:      "stdlib_signal_during_gc.go",
			expectation: runtimeCapabilityKnownGap,
			note:        "not a signal-delivery gap: every signal is delivered, but the 5.2.1 GC-assist allocation recursion stalls the receiving goroutine for up to 36s, past the program's own 2s deadline",
		},
		{
			category:       "stdlib-signals",
			name:           "during-netpoll",
			source:         "stdlib_signal_during_netpoll.go",
			expectation:    runtimeCapabilityMustPass,
			requiresAFINET: true,
		},
		{
			category:    "stdlib-signals",
			name:        "atomic-contention",
			source:      "stdlib_signal_atomic_contention.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:       "stdlib-http",
			name:           "client-server",
			source:         "stdlib_http_client_server.go",
			expectation:    runtimeCapabilityMustPass,
			requiresAFINET: true,
		},
		{
			category:    "stdlib-http",
			name:        "parse-roundtrip",
			source:      "stdlib_http_parse_roundtrip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-http",
			name:        "cookiejar",
			source:      "stdlib_http_cookiejar.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-http",
			name:        "multipart-form",
			source:      "stdlib_http_multipart_form.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:       "stdlib-http",
			name:           "tls-client-server",
			source:         "stdlib_http_tls_client_server.go",
			expectation:    runtimeCapabilityMustPass,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-http",
			name:           "redirect-keepalive",
			source:         "stdlib_http_redirect_keepalive.go",
			expectation:    runtimeCapabilityMustPass,
			requiresAFINET: true,
		},
		{
			category:    "stdlib-sync",
			name:        "once-map-cond",
			source:      "stdlib_sync_once_map_cond.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-sync",
			name:        "once-func",
			source:      "runtime_sync_once_func.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-sync",
			name:        "atomic-typed",
			source:      "stdlib_sync_atomic_typed.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-runtime-values",
			name:        "unique-handle",
			source:      "stdlib_unique_handle.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-runtime-values",
			name:        "weak-pointer",
			source:      "stdlib_weak_pointer.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-path",
			name:        "match-clean",
			source:      "stdlib_path_match_clean.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-unicode",
			name:        "utf8-decode",
			source:      "stdlib_unicode_utf8_decode.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-math",
			name:        "big-rat-int",
			source:      "stdlib_math_big_rat_int.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-math",
			name:        "rand-v2",
			source:      "stdlib_rand_v2.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-hash",
			name:        "crc32-fnv",
			source:      "stdlib_hash_crc32_fnv.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-hash",
			name:        "adler32-state",
			source:      "adler32_marshal_loop.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-generics",
			name:        "cmp-order",
			source:      "stdlib_cmp_order.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-generics",
			name:        "iter-pull",
			source:      "stdlib_iter_pull.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-io",
			name:        "ioutil-tempfile",
			source:      "stdlib_ioutil_tempfile.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-runtime-diagnostics",
			name:        "debug-stack-gc",
			source:      "stdlib_runtime_debug_stack_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-runtime-diagnostics",
			name:        "metrics-read",
			source:      "stdlib_runtime_metrics_read.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-runtime-diagnostics",
			name:        "trace-start-stop",
			source:      "stdlib_runtime_trace_start_stop.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-runtime-diagnostics",
			name:        "trace-start-only",
			source:      "stdlib_runtime_trace_start_only.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-runtime-diagnostics",
			name:        "trace-start-probe",
			source:      "stdlib_runtime_trace_start_probe.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-runtime-diagnostics",
			name:        "trace-log",
			source:      "stdlib_runtime_trace_log.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-runtime-diagnostics",
			name:        "trace-buffer",
			source:      "stdlib_runtime_trace_buffer.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-runtime-diagnostics",
			name:        "allocs-per-run",
			source:      "allocs_per_run.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-os-process",
			name:        "exec-echo",
			source:      "stdlib_os_exec_echo.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-netpoll",
			name:        "pipe-past-deadline",
			source:      "stdlib_netpoll_pipe_past_deadline.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-netpoll",
			name:        "pipe-deadline",
			source:      "stdlib_netpoll_pipe_deadline.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-netpoll",
			name:        "pipe-close-unblocks-read",
			source:      "stdlib_netpoll_pipe_close_unblocks_read.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-netpoll",
			name:        "pipe-afterfunc-close",
			source:      "stdlib_netpoll_pipe_afterfunc_close.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-netpoll",
			name:        "pipe-roundtrip",
			source:      "stdlib_netpoll_pipe_roundtrip.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-netpoll-stress",
			name:        "pipe-deadline-reset",
			source:      "stdlib_netpoll_stress_pipe_deadline_reset.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stdlib-netpoll-stress",
			name:        "pipe-close-churn",
			source:      "stdlib_netpoll_stress_pipe_close_churn.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:       "stdlib-netpoll-stress",
			name:           "tcp-churn",
			source:         "stdlib_netpoll_stress_tcp_churn.go",
			expectation:    runtimeCapabilityMustPass,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-netpoll-stress",
			name:           "udp-burst",
			source:         "stdlib_netpoll_stress_udp_burst.go",
			expectation:    runtimeCapabilityMustPass,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-netpoll",
			name:           "syscall-socket-listen",
			source:         "stdlib_netpoll_syscall_socket_listen.go",
			expectation:    runtimeCapabilityMustPass,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-netpoll",
			name:           "tcp-echo",
			source:         "stdlib_netpoll_tcp_echo.go",
			expectation:    runtimeCapabilityMustPass,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-netpoll",
			name:           "tcp-read-deadline",
			source:         "stdlib_netpoll_tcp_read_deadline.go",
			expectation:    runtimeCapabilityMustPass,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-netpoll",
			name:           "tcp-accept-deadline",
			source:         "stdlib_netpoll_tcp_accept_deadline.go",
			expectation:    runtimeCapabilityMustPass,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-netpoll",
			name:           "tcp-concurrent-clients",
			source:         "stdlib_netpoll_tcp_concurrent_clients.go",
			expectation:    runtimeCapabilityMustPass,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-netpoll",
			name:           "udp-loopback",
			source:         "stdlib_netpoll_udp_loopback.go",
			expectation:    runtimeCapabilityMustPass,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-netpoll",
			name:           "udp-deadline",
			source:         "stdlib_netpoll_udp_deadline.go",
			expectation:    runtimeCapabilityMustPass,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-netpoll",
			name:           "close-unblocks-read",
			source:         "stdlib_netpoll_close_unblocks_read.go",
			expectation:    runtimeCapabilityMustPass,
			requiresAFINET: true,
		},
		{
			category:    "defer-panic",
			name:        "panic-string-output",
			source:      "runtime_panic_print_string.go",
			expectation: runtimeCapabilityExpectedFailure,
			output:      "panic: boom",
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
			name:        "error-recover",
			source:      "runtime_panic_error_recover.go",
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
			category:    "defer-panic",
			name:        "defer-method-value-order",
			source:      "runtime_defer_method_value_order.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "panic-nil-recover",
			source:      "runtime_panic_nil_recover.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "panic-deep-unwind",
			source:      "runtime_panic_deep_unwind.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "panic-struct-recover",
			source:      "runtime_panic_struct_recover.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "defer-named-result-panic",
			source:      "runtime_defer_named_result_panic.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "defer-panic",
			name:        "panic-alloc-recover-gc",
			source:      "runtime_panic_alloc_recover_gc.go",
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
			name:        "reflect-value-int",
			source:      "runtime_reflect_value_int.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-value-indirect-call",
			source:      "runtime_reflect_value_indirect_call.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-value-instruction-op",
			source:      "runtime_reflect_value_instruction_op.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-call-aggregate",
			source:      "runtime_reflect_call_aggregate.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-call-aggregate-probe",
			source:      "runtime_reflect_call_aggregate_probe.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-call-aggregate-matrix",
			source:      "runtime_reflect_call_aggregate_matrix.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-call-aggregate-function",
			source:      "runtime_reflect_call_aggregate_function_probe.go",
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
			name:        "reflect-interface-extract-probe",
			source:      "runtime_reflect_interface_extract_probe.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "reflect-type-assert",
			source:      "runtime_reflect_type_assert_probe.go",
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
			category:    "runtime-packages",
			name:        "sync-mutex-defer",
			source:      "runtime_sync_mutex_defer.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "finalizer-resurrect",
			source:      "runtime_finalizer_resurrect.go",
			expectation: runtimeCapabilityKnownGap,
			note:        "the interface temporary built for SetFinalizer is a stack aggregate whose address reaches a destructed phi, so its safepoint maps stay conservative and its data word keeps the object reachable (see RUNTIME_PLAN.md 5.3)",
		},
		{
			category:    "runtime-packages",
			name:        "sync-rwmutex-readers",
			source:      "runtime_sync_rwmutex_readers.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "timer-gc-channel",
			source:      "runtime_timer_gc_channel.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "sync-once-concurrent",
			source:      "runtime_sync_once_concurrent.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "ticker-multi-tick",
			source:      "runtime_ticker_multi_tick.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "waitgroup-reuse",
			source:      "runtime_waitgroup_reuse.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "timer-callback-shape",
			source:      "runtime_timer_callback_shape.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "runtime-packages",
			name:        "timer-reset-after-drain",
			source:      "runtime_timer_reset_after_drain.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stack",
			name:        "panic-stack-gc",
			source:      "runtime_panic_stack_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "stack",
			name:        "many-defers-stack",
			source:      "runtime_many_defers_stack.go",
			expectation: runtimeCapabilityMustPass,
		},
	}

	for index, capability := range capabilities {
		capability := capability
		// Shard the matrix across parallel CI jobs: each shard owns the
		// capabilities whose index is congruent to it modulo the shard count.
		// Index-based partitioning keeps the shards balanced and stays correct
		// as capabilities are added, unlike a category-name filter.
		if index%*runtimeStatusShards != *runtimeStatusShard {
			continue
		}
		t.Run(capability.category+"/"+capability.name, func(t *testing.T) {
			if capability.requiresAFINET && !afinetSocketAvailable {
				t.Skipf("AF_INET sockets unavailable in this execution environment: %v", afinetSocketErr)
			}
			if *runtimeStatusProgress {
				fmt.Fprintf(os.Stderr, "runtime-status: start %s/%s %s\n", capability.category, capability.name, capability.source)
			}
			result := runRuntimeCapabilityProgram(t, compiler, directory, capability)
			if *runtimeStatusProgress {
				status := "pass"
				if result.err != nil {
					status = "fail"
				}
				fmt.Fprintf(
					os.Stderr,
					"runtime-status: %s %s/%s compile=%s %s peak=%.1fMiB run=%s %s peak=%.1fMiB coverage=%s\n",
					status,
					capability.category,
					capability.name,
					result.compileOutcome,
					result.compileDuration.Round(time.Millisecond),
					float64(result.compilePeakRSS)/(1024*1024),
					result.runOutcome,
					result.runDuration.Round(time.Millisecond),
					float64(result.runPeakRSS)/(1024*1024),
					result.coverageOutcome,
				)
			}
			runtimeCoverageCollector.add(capability, result)
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
			if capability.expectation == runtimeCapabilityExpectedFailure {
				if result.err == nil {
					t.Fatalf("%s should fail", capability.source)
				}
				if capability.output != "" && !strings.Contains(result.output, capability.output) {
					t.Fatalf("%s failed without expected output %q:\n%s", capability.source, capability.output, result.output)
				}
				t.Logf("EXPECTED FAILURE %s", capability.source)
				return
			}
			t.Logf("PASS %s", capability.source)
		})
	}
	runtimeCoverageCollector.write(t)
}

func truncateRuntimeCapabilityOutput(output string) string {
	const limit = 2000
	if len(output) <= limit {
		return output
	}
	return output[:limit] + "\n... truncated ..."
}

func runtimeCapabilityAFINETSocketAvailable() (bool, error) {
	fileDescriptor, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return false, err
	}
	if err := syscall.Close(fileDescriptor); err != nil {
		return false, err
	}
	return true, nil
}

type runtimeCapabilityResult struct {
	output          string
	err             error
	coverage        *runtimeCapabilityCoverageResult
	compileOutcome  string
	runOutcome      string
	coverageOutcome string
	coverageErr     error
	compileDuration time.Duration
	compilePeakRSS  uint64
	runDuration     time.Duration
	runPeakRSS      uint64
	runAttempts     int
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
	defer func() {
		if err := os.Remove(executable); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Logf("remove runtime capability executable: %v", err)
		}
	}()

	compileArguments := []string{"-o", executable}
	if *runtimeOptimize {
		compileArguments = append(compileArguments, "-O")
	}
	metadata := ""
	if *runtimeCoverageProfile != "" {
		metadata = executable + ".runtime-cover.json"
		defer os.Remove(metadata)
		compileArguments = append(compileArguments, "-runtime-covermeta", metadata)
	}
	compileArguments = append(compileArguments, source)
	compile := exec.Command(compiler, compileArguments...)
	compileStarted := time.Now()
	compileOutput, compileErr := compile.CombinedOutput()
	compileDuration := time.Since(compileStarted)
	compilePeakRSS := runtimeCapabilityPeakRSS(compile)
	if compileErr != nil {
		return runtimeCapabilityResult{
			output:          string(compileOutput),
			err:             errors.New("compile failed: " + compileErr.Error()),
			compileOutcome:  "failed",
			runOutcome:      "not-run",
			coverageOutcome: "not-run",
			compileDuration: compileDuration,
			compilePeakRSS:  compilePeakRSS,
		}
	}

	timeout := capability.timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	runCount := *runtimeStatusRuns
	if metadata != "" {
		runCount = max(runCount, *runtimeCoverageRuns)
	}
	var output []byte
	var err error
	var coverageResult *runtimeCapabilityCoverageResult
	var coverageErr error
	coverageOutcome := "not-requested"
	runOutcome := "passed"
	var runDuration time.Duration
	var runPeakRSS uint64
	runAttempts := 0
	for attempt := 0; attempt < runCount; attempt++ {
		runAttempts++
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		run := exec.CommandContext(ctx, executable)
		run.Env = runtimeCapabilityExecutionEnv()
		runStarted := time.Now()
		attemptOutput, attemptErr := run.CombinedOutput()
		runDuration += time.Since(runStarted)
		runPeakRSS = max(runPeakRSS, runtimeCapabilityPeakRSS(run))
		if ctx.Err() != nil {
			attemptErr = ctx.Err()
			runOutcome = "timeout"
		} else if attemptErr != nil {
			runOutcome = "failed"
		}
		cancel()

		if metadata != "" {
			var attemptCoverage *runtimeCapabilityCoverageResult
			attemptCoverage, attemptOutput, coverageErr = readRuntimeCapabilityCoverage(metadata, attemptOutput)
			if coverageErr != nil {
				coverageOutcome = "missing"
				if attemptErr == nil {
					attemptErr = coverageErr
				}
			} else {
				coverageOutcome = "collected"
				coverageErr = mergeRuntimeCapabilityCoverage(&coverageResult, attemptCoverage)
				if coverageErr != nil {
					coverageOutcome = "missing"
					if attemptErr == nil {
						attemptErr = coverageErr
					}
				}
			}
		}
		output = attemptOutput
		err = attemptErr
		if err != nil {
			break
		}
	}

	return runtimeCapabilityResult{
		output:          string(bytes.TrimSpace(output)),
		err:             err,
		coverage:        coverageResult,
		compileOutcome:  "passed",
		runOutcome:      runOutcome,
		coverageOutcome: coverageOutcome,
		coverageErr:     coverageErr,
		compileDuration: compileDuration,
		compilePeakRSS:  compilePeakRSS,
		runDuration:     runDuration,
		runPeakRSS:      runPeakRSS,
		runAttempts:     runAttempts,
	}
}

func mergeRuntimeCapabilityCoverage(
	merged **runtimeCapabilityCoverageResult,
	attempt *runtimeCapabilityCoverageResult,
) error {
	if *merged == nil {
		*merged = attempt
		return nil
	}
	if (*merged).metadata.RuntimeSourceID != attempt.metadata.RuntimeSourceID {
		return fmt.Errorf(
			"runtime source changed between coverage runs: %s then %s",
			(*merged).metadata.RuntimeSourceID,
			attempt.metadata.RuntimeSourceID,
		)
	}
	if len((*merged).hits) != len(attempt.hits) {
		return fmt.Errorf("runtime coverage bitmap changed size between runs: %d then %d", len((*merged).hits), len(attempt.hits))
	}
	for index, hit := range attempt.hits {
		(*merged).hits[index] = (*merged).hits[index] || hit
	}
	return nil
}

func runtimeCapabilityPeakRSS(command *exec.Cmd) uint64 {
	if command.ProcessState == nil {
		return 0
	}
	usage, ok := command.ProcessState.SysUsage().(*syscall.Rusage)
	if !ok || usage.Maxrss <= 0 {
		return 0
	}
	return uint64(usage.Maxrss) * 1024
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

	filtered = append(filtered, fmt.Sprintf("GOMAXPROCS=%d", *runtimeProcs))
	return filtered
}
