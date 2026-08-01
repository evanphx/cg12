package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	// exclusive marks a capability that must run with nothing else running.
	//
	// The run phase runs everything else concurrently, so a program whose
	// outcome depends on how much of the machine it has -- it measures or waits
	// on wall clock, sets an I/O deadline, asserts an allocation or GC
	// statistic, sets its own GOMAXPROCS, or deliberately saturates the
	// scheduler -- has to be marked or it becomes flaky, and a flaky suite is
	// worse than a slow one. TestRuntimeCapabilityExclusiveClassification
	// enforces that floor mechanically from the program's source; the field also
	// carries markings made for reasons no pattern finds.
	exclusive bool
	// termination records how the program is meant to end. A program that
	// deliberately terminates abnormally may lose its coverage packet, and that
	// absence is a classified outcome instead of a collection failure.
	termination     runtimeCapabilityTermination
	terminationNote string
	// env holds "NAME=value" entries added to the program's environment. It
	// exists so a capability can run under a runtime diagnostic -- the stack
	// scanning capabilities run under GODEBUG=cg12scanroots or
	// cg12checkstackcopy, and gc-invariants/checkmark under gccheckmark --
	// without the program having to re-execute itself to set one. Entries are
	// appended after the inherited environment, so they win over an inherited
	// value, including the GOMAXPROCS the run phase sets.
	env []string
}

type runtimeCapabilityExpectation int

const (
	runtimeCapabilityMustPass runtimeCapabilityExpectation = iota
	runtimeCapabilityKnownGap
	runtimeCapabilityExpectedFailure
)

// runtimeCapabilityTermination classifies how a capability program is expected
// to leave the process, which decides whether a missing coverage packet is a
// collection failure or an explicitly classified absence.
type runtimeCapabilityTermination int

const (
	// runtimeCapabilityTerminatesNormally is the default: the program returns
	// from main, so its coverage packet must be present.
	runtimeCapabilityTerminatesNormally runtimeCapabilityTermination = iota
	// runtimeCapabilityTerminatesAbnormally marks a deliberate uncaught panic,
	// throw, or fatal subprocess path. cg12 emits the coverage dump ahead of
	// every runtime.exit call, so these programs usually still deliver a
	// packet; the classification records that losing one is not a defect.
	runtimeCapabilityTerminatesAbnormally
)

// runtimeCapabilities returns the complete Linux/ARM64 runtime capability
// matrix. It is the single denominator for both the capability status suite and
// the runtime coverage corpus: a coverage run reports one explicit outcome per
// entry returned here, so the two counts can never drift apart silently.
func runtimeCapabilities() []runtimeCapability {
	return []runtimeCapability{
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
			category:    "core-types",
			name:        "complex64-parts",
			source:      "runtime_complex64_parts.go",
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
			exclusive:   true,
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
			exclusive:   true,
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
			exclusive:   true,
		},
		{
			category:    "gc",
			name:        "cleanup-basic",
			source:      "runtime_cleanup_basic.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "gc",
			name:        "cleanup-stop",
			source:      "runtime_cleanup_stop.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "gc",
			name:        "cleanup-multiple",
			source:      "runtime_cleanup_multiple.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "gc",
			name:        "cleanup-frame-retention",
			source:      "runtime_cleanup_frame_retention.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "gc",
			name:        "assist-alloc-recursion",
			source:      "runtime_gc_assist_stack_growth.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "gc",
			name:        "defer-capture-allocs",
			source:      "runtime_defer_capture_allocs.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "gc",
			name:        "cleanup-frame-retention-masked",
			source:      "runtime_cleanup_frame_retention_masked.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "gc",
			name:        "cleanup-frame-retention-scribble",
			source:      "runtime_cleanup_frame_retention_scribble.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category: "gc",
			name:     "span-metadata-barrier",
			source:   "runtime_span_metadata_barrier.go",
			// The only capability that raises GOMAXPROCS itself. Its failure
			// mode needs spans grown during marking from the initial thread's
			// g0 stack, which -runtime-procs=1..4 never produces. See 5.2.2.
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "gc",
			name:        "stack-argument-roots",
			source:      "runtime_stack_argument_roots.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category: "gc",
			name:     "keepalive-stack-root",
			source:   "runtime_keepalive_stack_root.go",
			// The reducer for "found bad pointer in Go heap". Like
			// span-metadata-barrier it raises GOMAXPROCS itself, because the
			// failure needs several goroutine stacks to move per collection and
			// -runtime-procs only goes up to 4. Probabilistic: about one run in
			// three before the fix at every -runtime-procs setting, none after.
			// See RUNTIME_PLAN.md 5.8.
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category: "gc",
			name:     "goroutine-entry-stack-map",
			source:   "runtime_goroutine_entry_stack_map.go",
			// The reducer for "found pointer to free object". Like
			// keepalive-stack-root it sets GOMAXPROCS and GOGC itself: the failure
			// needs many more goroutines than Ps so that a collection catches
			// goroutines still stopped at their entry pc. Probabilistic: about 92
			// runs in 100 before the fix at -O, none in several thousand after.
			// See RUNTIME_PLAN.md 5.11.
			timeout:     90 * time.Second,
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "gc",
			name:        "finalizer-cleanup-order",
			source:      "runtime_finalizer_cleanup_order.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "gc",
			name:        "finalizer-dependency-order",
			source:      "runtime_finalizer_dependency_order.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "gc",
			name:        "finalizer-tiny",
			source:      "runtime_finalizer_tiny.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "gc",
			name:        "pinner-lifecycle",
			source:      "runtime_pinner_lifecycle.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
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
			category:    "goroutine",
			name:        "gosched-progress",
			source:      "runtime_gosched_progress.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
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
			exclusive:   true,
		},
		{
			category:    "scheduler-stress",
			name:        "timer-select-churn",
			source:      "runtime_timer_select_churn.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "scheduler-stress",
			name:        "gc-churn",
			source:      "runtime_scheduler_gc_churn.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
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
			exclusive:   true,
		},
		{
			category:    "stdlib-bytes",
			name:        "grow-stats",
			source:      "bytes_grow_stats.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "stdlib-bytes",
			name:        "grow-allocs",
			source:      "bytes_grow_allocs.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "stdlib-bytes",
			name:        "replace-allocs",
			source:      "bytes_replace_allocs.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
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
			exclusive:   true,
		},
		{
			category:    "stdlib-signals",
			name:        "repeated-stop-reset",
			source:      "stdlib_signal_repeated_stop_reset.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "stdlib-signals",
			name:        "during-gc",
			source:      "stdlib_signal_during_gc.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:       "stdlib-signals",
			name:           "during-netpoll",
			source:         "stdlib_signal_during_netpoll.go",
			expectation:    runtimeCapabilityMustPass,
			exclusive:      true,
			requiresAFINET: true,
		},
		{
			category:    "stdlib-signals",
			name:        "atomic-contention",
			source:      "stdlib_signal_atomic_contention.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
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
			exclusive:   true,
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
			exclusive:   true,
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
			exclusive:   true,
		},
		{
			category:    "stdlib-netpoll",
			name:        "pipe-deadline",
			source:      "stdlib_netpoll_pipe_deadline.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "stdlib-netpoll",
			name:        "pipe-close-unblocks-read",
			source:      "stdlib_netpoll_pipe_close_unblocks_read.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "stdlib-netpoll",
			name:        "pipe-afterfunc-close",
			source:      "stdlib_netpoll_pipe_afterfunc_close.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
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
			exclusive:   true,
		},
		{
			category:    "stdlib-netpoll-stress",
			name:        "pipe-close-churn",
			source:      "stdlib_netpoll_stress_pipe_close_churn.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:       "stdlib-netpoll-stress",
			name:           "tcp-churn",
			source:         "stdlib_netpoll_stress_tcp_churn.go",
			expectation:    runtimeCapabilityMustPass,
			exclusive:      true,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-netpoll-stress",
			name:           "udp-burst",
			source:         "stdlib_netpoll_stress_udp_burst.go",
			expectation:    runtimeCapabilityMustPass,
			exclusive:      true,
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
			exclusive:      true,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-netpoll",
			name:           "tcp-read-deadline",
			source:         "stdlib_netpoll_tcp_read_deadline.go",
			expectation:    runtimeCapabilityMustPass,
			exclusive:      true,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-netpoll",
			name:           "tcp-accept-deadline",
			source:         "stdlib_netpoll_tcp_accept_deadline.go",
			expectation:    runtimeCapabilityMustPass,
			exclusive:      true,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-netpoll",
			name:           "tcp-concurrent-clients",
			source:         "stdlib_netpoll_tcp_concurrent_clients.go",
			expectation:    runtimeCapabilityMustPass,
			exclusive:      true,
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
			exclusive:      true,
			requiresAFINET: true,
		},
		{
			category:       "stdlib-netpoll",
			name:           "close-unblocks-read",
			source:         "stdlib_netpoll_close_unblocks_read.go",
			expectation:    runtimeCapabilityMustPass,
			exclusive:      true,
			requiresAFINET: true,
		},
		{
			category:    "loop-variables",
			name:        "three-clause",
			source:      "runtime_loopvar_three_clause.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "loop-variables",
			name:        "range-forms",
			source:      "runtime_loopvar_range.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "loop-variables",
			name:        "goroutine-and-defer",
			source:      "runtime_loopvar_goroutine_defer.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "loop-variables",
			name:        "address-gc",
			source:      "runtime_loopvar_address_gc.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "loop-variables",
			name:        "value-shapes",
			source:      "runtime_loopvar_value_shapes.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "loop-variables",
			name:        "shared-scope",
			source:      "runtime_loopvar_shared_scope.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "print-builtin",
			name:        "operand-separation",
			source:      "runtime_println_operand_separation.go",
			expectation: runtimeCapabilityMustPass,
		},
		{
			category:    "print-builtin",
			name:        "statement-atomicity",
			source:      "runtime_println_statement_atomicity.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "assignment-targets",
			name:        "range-target-forms",
			source:      "runtime_range_target_forms.go",
			expectation: runtimeCapabilityMustPass,
			output:      "range target forms ok",
		},
		{
			category:    "assignment-targets",
			name:        "range-target-order",
			source:      "runtime_range_target_order.go",
			expectation: runtimeCapabilityMustPass,
			output:      "range target order ok",
		},
		{
			category:    "assignment-targets",
			name:        "multi-assignment-forms",
			source:      "runtime_assign_target_forms.go",
			expectation: runtimeCapabilityMustPass,
			output:      "assign target forms ok",
		},
		{
			category:        "defer-panic",
			name:            "panic-string-output",
			source:          "runtime_panic_print_string.go",
			expectation:     runtimeCapabilityExpectedFailure,
			output:          "panic: boom",
			termination:     runtimeCapabilityTerminatesAbnormally,
			terminationNote: "the program deliberately panics without recovering, so the process dies on the unwind path",
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
			exclusive:   true,
		},
		{
			category:    "runtime-packages",
			name:        "gomaxprocs-memstats",
			source:      "gomaxprocs_memstats.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "runtime-packages",
			name:        "time-reset-stop",
			source:      "runtime_timer_reset_stop.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "runtime-packages",
			name:        "ticker-stop",
			source:      "runtime_ticker_stop.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
		},
		{
			category:    "runtime-packages",
			name:        "select-timeout",
			source:      "runtime_select_timeout.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
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
			exclusive:   true,
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
			exclusive:   true,
		},
		{
			category:    "runtime-packages",
			name:        "time-afterfunc",
			source:      "runtime_time_afterfunc.go",
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
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
			exclusive:   true,
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
			expectation: runtimeCapabilityMustPass,
			exclusive:   true,
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
			exclusive:   true,
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
			exclusive:   true,
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
			exclusive:   true,
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

		// Stack scanning, RUNTIME_PLAN.md section 6. Each program keeps its
		// objects reachable only through the frame under test and observes a
		// premature collection rather than inferring one. The diagnostics they
		// run under are the ones Phase 1 built: cg12scanroots names the frame
		// and stack-map slot that retains each object, and cg12checkstackcopy
		// throws at a stale old-stack pointer instead of leaving it to be found
		// later.
		{
			category:    "stack-scan",
			name:        "loop-safepoints",
			source:      "runtime_stack_scan_loop_safepoints.go",
			expectation: runtimeCapabilityMustPass,
			env:         []string{"GODEBUG=cg12scanroots=1"},
			timeout:     120 * time.Second,
			exclusive:   true,
		},
		{
			category:    "stack-scan",
			name:        "blocked-goroutines",
			source:      "runtime_stack_scan_blocked_goroutines.go",
			expectation: runtimeCapabilityMustPass,
			env:         []string{"GODEBUG=cg12scanroots=1"},
			timeout:     120 * time.Second,
			exclusive:   true,
		},
		{
			category:    "stack-scan",
			name:        "syscall-transitions",
			source:      "runtime_stack_scan_syscall.go",
			expectation: runtimeCapabilityMustPass,
			env:         []string{"GODEBUG=cg12scanroots=1"},
			timeout:     120 * time.Second,
			exclusive:   true,
		},
		{
			category:    "stack-scan",
			name:        "panic-unwind",
			source:      "runtime_stack_scan_panic_unwind.go",
			expectation: runtimeCapabilityMustPass,
			env:         []string{"GODEBUG=cg12scanroots=1"},
			timeout:     120 * time.Second,
			exclusive:   true,
		},
		{
			category:    "stack-scan",
			name:        "stack-copy-roots",
			source:      "runtime_stack_copy_roots.go",
			expectation: runtimeCapabilityMustPass,
			env:         []string{"GODEBUG=cg12checkstackcopy=1"},
			timeout:     180 * time.Second,
			exclusive:   true,
		},
		{
			category:    "stack-scan",
			name:        "callfree-loop-roots",
			source:      "runtime_stack_scan_callfree_loop.go",
			expectation: runtimeCapabilityMustPass,
			env:         []string{"GOMAXPROCS=4"},
			timeout:     180 * time.Second,
			exclusive:   true,
		},

		// The channel buffer is a GC root. This is the capability that found the
		// defect in goc's channel element descriptor: makechan reads the
		// element's PtrBytes to decide whether the buffer needs to be a
		// scannable allocation at all, so a stub descriptor put every buffered
		// element somewhere the mark phase never looked.
		{
			category:    "gc",
			name:        "channel-buffer-roots",
			source:      "runtime_channel_buffer_roots.go",
			expectation: runtimeCapabilityMustPass,
			env:         []string{"GODEBUG=clobberfree=1"},
			timeout:     120 * time.Second,
			exclusive:   true,
		},

		// GC stress, RUNTIME_PLAN.md section 6: concurrent allocation during
		// marking, assist work, sweep pacing, scavenging, growth and shrink
		// cycles, and low-memory pressure. All exclusive: every one of them
		// asserts an allocation or GC statistic, changes a process-wide runtime
		// limit, or deliberately saturates the allocator.
		{
			category:    "gc-stress",
			name:        "concurrent-mark",
			source:      "runtime_gc_concurrent_mark.go",
			expectation: runtimeCapabilityMustPass,
			env:         []string{"GODEBUG=cg12checkwb=2"},
			timeout:     240 * time.Second,
			exclusive:   true,
		},
		{
			category:    "gc-stress",
			name:        "assist-credit",
			source:      "runtime_gc_assist_credit.go",
			expectation: runtimeCapabilityMustPass,
			timeout:     240 * time.Second,
			exclusive:   true,
		},
		{
			category:    "gc-stress",
			name:        "sweep-pacing",
			source:      "runtime_gc_sweep_pacing.go",
			expectation: runtimeCapabilityMustPass,
			timeout:     240 * time.Second,
			exclusive:   true,
		},
		{
			category:    "gc-stress",
			name:        "scavenge-release",
			source:      "runtime_gc_scavenge_release.go",
			expectation: runtimeCapabilityMustPass,
			timeout:     240 * time.Second,
			exclusive:   true,
		},
		{
			category:    "gc-stress",
			name:        "heap-growth-shrink",
			source:      "runtime_gc_heap_growth_shrink.go",
			expectation: runtimeCapabilityMustPass,
			timeout:     240 * time.Second,
			exclusive:   true,
		},
		{
			category:    "gc-stress",
			name:        "memory-limit",
			source:      "runtime_gc_memory_limit.go",
			expectation: runtimeCapabilityMustPass,
			timeout:     240 * time.Second,
			exclusive:   true,
		},

		// Rare invariant paths, RUNTIME_PLAN.md section 6. checkmark re-marks
		// the whole heap with the world stopped and throws at any object the
		// concurrent phase missed; the mark-worker and huge-page programs reach
		// paths no Go program can observe from inside itself, and what they
		// reached is asserted separately against the runtime coverage bitmap by
		// cmd/goc/runtime_gc_paths_test.go.
		{
			category:    "gc-invariants",
			name:        "checkmark",
			source:      "runtime_gc_checkmark.go",
			expectation: runtimeCapabilityMustPass,
			env:         []string{"GODEBUG=gccheckmark=1"},
			timeout:     240 * time.Second,
			exclusive:   true,
		},
		{
			category:    "gc-invariants",
			name:        "mark-workers",
			source:      "runtime_gc_mark_workers.go",
			expectation: runtimeCapabilityMustPass,
			env:         []string{"GOMAXPROCS=3"},
			timeout:     180 * time.Second,
			exclusive:   true,
		},
		{
			category:    "gc-invariants",
			name:        "metadata-hugepages",
			source:      "runtime_gc_metadata_hugepages.go",
			expectation: runtimeCapabilityMustPass,
			timeout:     240 * time.Second,
			exclusive:   true,
		},
	}
}

func TestARM64RuntimeCapabilityStatus(t *testing.T) {
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
	// A coverage report describes the whole corpus: its summary, its
	// classification denominator, and the baseline it is compared against are
	// corpus-wide. A shard runs only part of the matrix and there is no report
	// merge, so a sharded coverage run would publish a fraction of the corpus
	// as though it were complete.
	if *runtimeCoverageProfile != "" && *runtimeStatusShards != 1 {
		t.Fatalf("-runtime-coverprofile requires an unsharded run, but -runtime-status-shards is %d", *runtimeStatusShards)
	}
	// A skipped matrix writes no report at all, which is indistinguishable from
	// a report that lost every program. Asking for coverage in an environment
	// that cannot run the corpus is an error rather than a skip.
	if runtime.GOARCH != "arm64" {
		if *runtimeCoverageProfile != "" {
			t.Fatalf("-runtime-coverprofile requires arm64, but this host is %s", runtime.GOARCH)
		}
		t.Skip("AArch64 Go runtime capability status")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		if *runtimeCoverageProfile != "" {
			t.Fatalf("-runtime-coverprofile requires a system cc: %v", err)
		}
		t.Skip("cc unavailable")
	}

	directory := t.TempDir()
	compiler := buildGOCForRuntimeCapabilityStatus(t, directory)
	afinetSocketAvailable, afinetSocketErr := runtimeCapabilityAFINETSocketAvailable()

	capabilities := runtimeCapabilities()
	runtimeCoverageCollector.expect(capabilities)

	// Compile this shard's programs up front and in parallel. Compilation is
	// 99.5% of this suite's compute -- roughly 3000 s of compiler time against a
	// 14 s run phase -- so overlapping it is the difference between tens of
	// minutes and a few.
	shard := make([]runtimeCapability, 0, len(capabilities))
	for index, capability := range capabilities {
		if index%*runtimeStatusShards != *runtimeStatusShard {
			continue
		}
		if !runtimeCapabilitySelected(capability) {
			continue
		}
		// A capability the environment cannot run is never compiled. Its
		// subtest skips before it would await a compilation, so queuing it
		// would occupy a look-ahead slot that nothing ever returns.
		if capability.requiresAFINET && !afinetSocketAvailable {
			continue
		}
		shard = append(shard, capability)
	}
	prebuiltRuntime := buildPrebuiltRuntimesForCapabilityStatus(t, compiler, directory)
	compileQueue := startRuntimeCapabilityCompiles(compiler, directory, prebuiltRuntime, shard)
	runner := &runtimeCapabilityRunner{
		compiler:              compiler,
		directory:             directory,
		prebuiltRuntime:       prebuiltRuntime,
		queue:                 compileQueue,
		slots:                 make(chan struct{}, runtimeCapabilityRunWorkers()),
		afinetSocketAvailable: afinetSocketAvailable,
		afinetSocketErr:       afinetSocketErr,
	}
	if *runtimeStatusProgress {
		fmt.Fprintf(
			os.Stderr,
			"runtime-status: compiling %d programs %d at a time; running %d concurrently, then %d exclusively\n",
			len(shard),
			compileRuntimeCapabilityWorkers(),
			runtimeCapabilityRunWorkers(),
			countExclusiveRuntimeCapabilities(shard),
		)
	}

	// The run phase has two halves that together cover the shard exactly once,
	// so the matrix still reports one subtest per capability.
	//
	// Running the non-exclusive half concurrently is worth about 7 s of run time
	// directly. It is worth far more indirectly: the look-ahead budget is
	// returned when a program *runs*, so a run phase that walked the matrix in
	// index order pinned the compile dispatcher to the run frontier, and a slow
	// compile in the middle of the matrix idled the workers behind it. See
	// RUNTIME_PLAN.md section 17.
	var concurrentRuns sync.WaitGroup
	for index, capability := range capabilities {
		capability := capability
		// Shard the matrix across parallel CI jobs: each shard owns the
		// capabilities whose index is congruent to it modulo the shard count.
		// Index-based partitioning keeps the shards balanced and stays correct
		// as capabilities are added, unlike a category-name filter.
		if index%*runtimeStatusShards != *runtimeStatusShard {
			continue
		}
		if capability.exclusive {
			continue
		}
		concurrentRuns.Add(1)
		go func() {
			defer concurrentRuns.Done()
			runner.run(t, capability)
		}()
	}
	concurrentRuns.Wait()

	// The exclusive programs run last, one at a time, with the compile queue
	// drained. Only their own compiles can still be outstanding at this point --
	// every other program has already run -- so waiting costs nothing and buys
	// them a machine with no compiler on it, which is more isolation than the
	// old sequential phase gave them.
	compileQueue.drainCompiles()
	for index, capability := range capabilities {
		capability := capability
		if index%*runtimeStatusShards != *runtimeStatusShard {
			continue
		}
		if !capability.exclusive {
			continue
		}
		runner.run(t, capability)
	}

	runtimeCoverageCollector.write(t)
}

// runtimeCapabilityRunner holds everything a capability subtest needs that does
// not vary between capabilities, so the concurrent half of the run phase and the
// exclusive half can share one body.
type runtimeCapabilityRunner struct {
	compiler              string
	directory             string
	prebuiltRuntime       string
	queue                 *runtimeCapabilityCompileQueue
	slots                 chan struct{}
	afinetSocketAvailable bool
	afinetSocketErr       error
}

// run executes one capability as a subtest. It is safe to call from several
// goroutines at once: testing.T.Run may be called concurrently as long as every
// call returns before the parent test function does, which both halves of the run
// phase guarantee.
func (runner *runtimeCapabilityRunner) run(t *testing.T, capability runtimeCapability) {
	t.Run(capability.category+"/"+capability.name, func(t *testing.T) {
		if capability.requiresAFINET && !runner.afinetSocketAvailable {
			// A skipped capability still owns a row in the coverage report.
			// Dropping it here would shrink the corpus denominator without
			// anything recording that it had.
			reason := fmt.Sprintf("AF_INET sockets unavailable in this execution environment: %v", runner.afinetSocketErr)
			runtimeCoverageCollector.add(capability, skippedRuntimeCapabilityResult(reason))
			t.Skip(reason)
		}
		if *runtimeStatusProgress {
			fmt.Fprintf(os.Stderr, "runtime-status: start %s/%s %s\n", capability.category, capability.name, capability.source)
		}
		compilation, queued := runner.queue.await(capability)
		if !queued {
			// -test.run and runtimeCapabilitySelected can disagree about
			// which subtests run, so a capability can reach here without
			// having been queued. Compile it here rather than assuming.
			compilation = compileRuntimeCapabilityWith(runner.compiler, runner.directory, runner.prebuiltRuntime, capability)
		} else {
			// The look-ahead budget must come back even if the run panics
			// or fails an assertion, or the dispatcher stalls behind it.
			defer runner.queue.release()
		}
		// The run slot is taken after the compilation is in hand. A subtest
		// waiting for its compile is not using the machine, and holding a slot
		// while it waits would cap how many programs can be waiting at once,
		// which is the coupling this phase exists to remove.
		releaseRunSlot := runner.acquireRunSlot(capability)
		defer releaseRunSlot()
		result := runRuntimeCapabilityProgram(t, compilation, capability)
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

// acquireRunSlot bounds how many capability programs execute at once, and
// returns the function that gives the slot back.
//
// An exclusive capability takes no slot. The half of the run phase that executes
// it is already serial, and the half that would contend with it has finished, so
// a slot would only be a second name for the same guarantee.
func (runner *runtimeCapabilityRunner) acquireRunSlot(capability runtimeCapability) func() {
	if capability.exclusive {
		return func() {}
	}
	runner.slots <- struct{}{}
	return func() {
		<-runner.slots
	}
}

// runtimeCapabilityRunWorkers is how many capability programs execute at once.
//
// This is deliberately not a lever. The whole run phase is about 14 s against
// roughly 3000 s of compile CPU, so a small number hides it completely, and the
// non-exclusive programs execute alongside a saturated compile queue -- keeping
// their number low is the other half of what the exclusive classification is
// protecting. Peak RSS over all 338 runs is 78 MiB, so memory is not the bound
// here the way it is for compilation.
func runtimeCapabilityRunWorkers() int {
	if *runtimeStatusRunWorkers > 0 {
		return *runtimeStatusRunWorkers
	}
	return 4
}

func countExclusiveRuntimeCapabilities(capabilities []runtimeCapability) int {
	exclusive := 0
	for _, capability := range capabilities {
		if capability.exclusive {
			exclusive++
		}
	}
	return exclusive
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
	// coverageReason names why no coverage packet was collected. Every outcome
	// other than runtimeCoverageOutcomeCollected carries one, so an absent
	// packet is always an explanation rather than an absence.
	coverageReason  string
	skipReason      string
	coverageErr     error
	compileDuration time.Duration
	compilePeakRSS  uint64
	runDuration     time.Duration
	runPeakRSS      uint64
	runAttempts     int
}

// skippedRuntimeCapabilityResult records a capability the environment could not
// run. It keeps the program in the corpus denominator with an explicit,
// self-describing outcome instead of removing it from the report.
func skippedRuntimeCapabilityResult(reason string) runtimeCapabilityResult {
	return runtimeCapabilityResult{
		compileOutcome:  runtimeCoverageOutcomeSkipped,
		runOutcome:      runtimeCoverageOutcomeSkipped,
		coverageOutcome: runtimeCoverageOutcomeSkipped,
		coverageReason:  reason,
		skipReason:      reason,
	}
}

// unreportedRuntimeCapabilityResult stands in for a capability the run never
// reached, which happens when the subtest aborts before it records anything.
// It is always a collection failure, but naming it keeps the capability in the
// report rather than letting the corpus lose a row without saying so.
func unreportedRuntimeCapabilityResult() runtimeCapabilityResult {
	const reason = "the capability produced no outcome of its own: the run ended before it reported one"
	return runtimeCapabilityResult{
		err:             errors.New(reason),
		compileOutcome:  runtimeCoverageOutcomeUnreported,
		runOutcome:      runtimeCoverageOutcomeUnreported,
		coverageOutcome: runtimeCoverageOutcomeMissing,
		coverageReason:  reason,
	}
}

// The capability matrix compiles one goc and reuses it for every program in the
// process. Building it is not free, and it used to happen with GOCACHE pointed
// at a fresh temporary directory, which forced a cold rebuild of the whole
// compiler on every invocation. The ambient build cache is correct here: `go
// build` already keys its cache on the source, so a stale binary is not a
// hazard it can produce.
var (
	runtimeCapabilityCompilerOnce sync.Once
	runtimeCapabilityCompilerPath string
	runtimeCapabilityCompilerErr  error
)

func buildGOCForRuntimeCapabilityStatus(t *testing.T, directory string) string {
	t.Helper()

	runtimeCapabilityCompilerOnce.Do(func() {
		compiler := filepath.Join(directory, "goc")
		// -buildvcs=false because the prebuilt runtime packs are cached against
		// this binary's bytes, and the stamp `go build` embeds by default carries
		// the commit and a clean/dirty bit. With it, every commit -- a report, a
		// comment -- rebuilds 157 s of packs that the compiler's own code did not
		// invalidate. Without it the binary is identified by what it compiles,
		// which is what the cache key means to say.
		build := exec.Command("go", "build", "-buildvcs=false", "-o", compiler, ".")
		if output, err := build.CombinedOutput(); err != nil {
			runtimeCapabilityCompilerErr = fmt.Errorf("build compiler: %w\n%s", err, output)
			return
		}
		runtimeCapabilityCompilerPath = compiler
	})
	if runtimeCapabilityCompilerErr != nil {
		t.Fatal(runtimeCapabilityCompilerErr)
	}

	return runtimeCapabilityCompilerPath
}

// runtimeCapabilityCompilation is one program's compile result, produced ahead
// of the run phase so the compiles can overlap.
type runtimeCapabilityCompilation struct {
	executable string
	metadata   string
	output     string
	err        error
	duration   time.Duration
	peakRSS    uint64
}

// compileRuntimeCapabilityPeakBytes is what one compile is assumed to need.
//
// It is measured rather than guessed: the largest net/http program peaks at
// 2.65 GiB inside the matrix and 2.97 GiB compiled on its own, so the 2 GiB this
// used to assume was under-provisioned by a third. Under-provisioning here is not
// a slowdown -- the bound exists so an unbounded fan-out cannot swap or OOM a
// small machine, and a divisor below the real peak lets it do exactly that.
const compileRuntimeCapabilityPeakBytes = 3 << 30

// compileRuntimeCapabilityWorkers is how many programs to compile at once.
//
// Compilation is 99.5% of this suite's compute and the programs are independent,
// so this is the difference between forty minutes and a few. It is bounded by
// memory as well as by cores, because a fan-out wide enough to use every core on
// a large machine needs tens of gigabytes to do it.
//
// More is not always faster. At 24 workers the matrix takes 203.2 s and spends
// 3428 s of compile CPU; at 64 it takes 204.4 s and spends 4228 s. The extra 40
// workers buy nothing, because the wall clock is bounded by one single-threaded
// 190 s compile, and they burn 23% more CPU on contention. The default stays at
// NumCPU because it has to be right on an eight-core machine too, but a caller
// with a CPU share to respect loses nothing by passing it.
func compileRuntimeCapabilityWorkers() int {
	if *runtimeStatusCompileWorkers > 0 {
		return *runtimeStatusCompileWorkers
	}
	workers := runtime.NumCPU()
	if available := availableMemoryBytes(); available > 0 {
		byMemory := int(available / compileRuntimeCapabilityPeakBytes)
		if byMemory < workers {
			workers = byMemory
		}
	}
	return max(workers, 1)
}

// availableMemoryBytes reports MemAvailable, or 0 when it cannot be read. It is
// deliberately MemAvailable rather than MemFree: page cache is reclaimable, and
// treating it as unavailable would serialize the suite on any machine that has
// been doing I/O.
func availableMemoryBytes() uint64 {
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(meminfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemAvailable:" {
			continue
		}
		kilobytes, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kilobytes * 1024
	}
	return 0
}

// runtimeCapabilityCompileQueue compiles programs ahead of the sequential run
// phase, bounded in two independent ways.
//
// Concurrency is bounded by compileRuntimeCapabilityWorkers, which is about CPU
// and memory. Look-ahead is bounded separately, because every compiled program
// occupies disk until its run deletes it: compiling all 338 up front peaks at
// well over a gigabyte, which is fine on a build host and fatal on a laptop
// whose /tmp is a tmpfs. Bounding the number of compiled-but-not-yet-run
// programs keeps the workers saturated -- the run phase is 0.5% of the suite's
// compute, so it never starves them -- while capping disk at the window rather
// than the corpus.
type runtimeCapabilityCompileQueue struct {
	entries   map[string]*runtimeCapabilityCompileEntry
	lookahead chan struct{}
	// batch is the pool of long-lived compilers, or nil when every program is
	// compiled by a `goc` process of its own.
	batch *runtimeCapabilityBatchPool
}

type runtimeCapabilityCompileEntry struct {
	done   chan struct{}
	result runtimeCapabilityCompilation
}

// runtimeCapabilityPackRoots are the package sets the matrix prebuilds, one pack
// each. goc picks, per program, the richest pack whose closure that program
// contains; the first entry carries nothing beyond the runtime and is therefore
// usable by every program, so the list degrades rather than failing.
//
// The choice is a measurement, not a taste. Compile cost across the matrix is
// sharply bimodal: eleven programs cost 125-167 s each and account for 54% of all
// compile CPU, while the other 327 average 4.2 s. These six roots are the largest
// package each of those eleven imports -- six net/http programs, one net/smtp, and
// four crypto programs that share no common ancestor small enough to be usable by
// all of them. Adding a root that no expensive program can use costs a pack build
// and saves nothing.
var runtimeCapabilityPackRoots = [][]string{
	nil,
	{"net/http"},
	{"net/smtp"},
	{"crypto/x509"},
	{"crypto/ecdsa"},
	{"crypto/ecdh"},
	{"crypto/hpke"},
}

// buildPrebuiltRuntimesForCapabilityStatus compiles the prebuilt runtime packs
// once for the whole run and returns them as goc's -runtime takes them, or ""
// when this run must compile the runtime per program.
//
// The packs are built concurrently because they are independent and each is a
// single-threaded goc process; a cold build of the whole set costs one net/http
// compile in wall clock rather than six. `goc build-runtime` caches its result on
// disk keyed on the compiler and the standard library, so past the first run of a
// given tree the whole step is a file copy.
func buildPrebuiltRuntimesForCapabilityStatus(t *testing.T, compiler, directory string) string {
	t.Helper()

	if !*runtimeStatusPrebuiltRuntime {
		return ""
	}
	if *runtimeCoverageProfile != "" {
		// Runtime coverage instruments the runtime per program, so there is
		// nothing shared to prebuild.
		return ""
	}
	roots := runtimeCapabilityPackRoots
	if !*runtimeStatusStdlibPacks {
		roots = roots[:1]
	}
	started := time.Now()
	packs := make([]string, len(roots))
	failures := make([]error, len(roots))
	var building sync.WaitGroup
	for index, root := range roots {
		packs[index] = filepath.Join(directory, fmt.Sprintf("runtime%d.gocrt", index))
		building.Add(1)
		go func(index int, root []string, output string) {
			defer building.Done()
			arguments := []string{"build-runtime", "-o", output, "-packages", strings.Join(root, ",")}
			if *runtimeOptimize {
				arguments = append(arguments, "-O")
			}
			build := exec.Command(compiler, arguments...)
			if output, err := build.CombinedOutput(); err != nil {
				failures[index] = fmt.Errorf("build the prebuilt runtime for %v: %w\n%s", root, err, output)
			}
		}(index, root, packs[index])
	}
	building.Wait()
	for _, err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if *runtimeStatusProgress {
		fmt.Fprintf(os.Stderr, "runtime-status: built %d prebuilt runtimes in %s\n",
			len(packs), time.Since(started).Round(time.Millisecond))
	}
	return strings.Join(packs, ",")
}

func startRuntimeCapabilityCompiles(
	compiler string,
	directory string,
	prebuiltRuntime string,
	capabilities []runtimeCapability,
) *runtimeCapabilityCompileQueue {
	workers := compileRuntimeCapabilityWorkers()

	// An exclusive capability holds its look-ahead budget from the moment it
	// compiles until the exclusive half of the run phase, at the very end, so
	// that budget is not reclaimable during the run. The window therefore has to
	// be the concurrent budget *plus* the exclusive count. Without that term this
	// is a deadlock rather than a slowdown: at 55 exclusive capabilities, any
	// worker count below 14 lets them alone exhaust a 4*workers window while the
	// dispatcher waits for a token only the final phase can return.
	exclusive := countExclusiveRuntimeCapabilities(capabilities)

	queue := &runtimeCapabilityCompileQueue{
		entries:   make(map[string]*runtimeCapabilityCompileEntry, len(capabilities)),
		lookahead: make(chan struct{}, 4*workers+exclusive),
		batch:     newRuntimeCapabilityBatchPoolFor(compiler, prebuiltRuntime, workers),
	}
	for _, capability := range capabilities {
		queue.entries[capability.category+"/"+capability.name] = &runtimeCapabilityCompileEntry{
			done: make(chan struct{}),
		}
	}

	// Dispatch the expensive programs first. Matrix order put eleven compiles of
	// 125-167 s each at indices 155-223 of 338, so they started late and left
	// workers idle behind them while the queue drained; a greedy list schedule
	// costs at most one job's length above the ideal, and this makes that job the
	// one that is going to bound the run anyway.
	ordered := runtimeCapabilitiesByDescendingCompileCost(capabilities)

	slots := make(chan struct{}, workers)
	go func() {
		for _, capability := range ordered {
			// Both budgets are taken before the compile starts and the
			// look-ahead one is only returned once the program has run, so a
			// slow run phase stalls compilation instead of filling the disk.
			queue.lookahead <- struct{}{}
			slots <- struct{}{}
			go func(capability runtimeCapability) {
				defer func() { <-slots }()
				entry := queue.entries[capability.category+"/"+capability.name]
				if queue.batch != nil {
					source, executable := runtimeCapabilityCompilePaths(directory, capability)
					entry.result = queue.batch.compile(source, executable)
				} else {
					entry.result = compileRuntimeCapabilityWith(compiler, directory, prebuiltRuntime, capability)
				}
				close(entry.done)
			}(capability)
		}
	}()

	return queue
}

// await blocks until the capability's program has been compiled. It reports
// false when the capability was never queued, which the caller must handle
// rather than blocking forever on a channel nothing will close.
func (queue *runtimeCapabilityCompileQueue) await(capability runtimeCapability) (runtimeCapabilityCompilation, bool) {
	entry, queued := queue.entries[capability.category+"/"+capability.name]
	if !queued {
		return runtimeCapabilityCompilation{}, false
	}
	<-entry.done
	return entry.result, true
}

// release returns the look-ahead budget a finished run was holding.
func (queue *runtimeCapabilityCompileQueue) release() {
	<-queue.lookahead
}

// drainCompiles waits for every queued compile to finish. The entry map is
// written once, before any compile starts, so reading it here needs no
// synchronization of its own.
func (queue *runtimeCapabilityCompileQueue) drainCompiles() {
	for _, entry := range queue.entries {
		<-entry.done
	}
	if queue.batch != nil {
		// Every compile has finished, so the workers are idle and holding a
		// parsed standard library and their packs. Shutting them down here
		// rather than at the end of the run gives the exclusive programs --
		// which run next, and are exclusive because their outcome depends on how
		// much of the machine they have -- a machine with no compilers on it at
		// all.
		queue.batch.stop()
	}
}

// runtimeCapabilitySelected reports whether -test.run would run this
// capability's subtest, so a targeted run compiles only what it will execute.
// Go matches each slash-separated element of the pattern against the
// corresponding element of the test name, unanchored, so this mirrors that.
// An unparseable pattern selects everything: over-compiling is slow, but
// skipping a capability the suite is about to run would report a phantom pass.
func runtimeCapabilitySelected(capability runtimeCapability) bool {
	pattern := ""
	if run := flag.Lookup("test.run"); run != nil {
		pattern = run.Value.String()
	}
	if pattern == "" {
		return true
	}

	elements := strings.Split(pattern, "/")
	name := []string{"TestARM64RuntimeCapabilityStatus", capability.category, capability.name}
	for index, element := range elements {
		if index >= len(name) {
			break
		}
		matched, err := regexp.MatchString(element, name[index])
		if err != nil {
			return true
		}
		if !matched {
			return false
		}
	}

	return true
}

// runtimeCapabilityCompilePaths is where a capability's source is read from and
// where its executable is written. Both compile paths use it, so a batch build
// and a one-shot build cannot drift into compiling different files.
func runtimeCapabilityCompilePaths(directory string, capability runtimeCapability) (source, executable string) {
	source = filepath.Join("..", "..", "goc", "testdata", capability.source)
	executable = filepath.Join(directory, strings.TrimSuffix(capability.source, ".go")+".bin")
	return source, executable
}

// newRuntimeCapabilityBatchPoolFor returns the pool of long-lived compilers this
// run should use, or nil when it must compile one program per process.
//
// The coverage run is the one that must not batch. It instruments the runtime
// per program through -runtime-covermeta, which `goc compile-batch` does not
// accept: a batch worker is one build configuration by construction, and
// per-program instrumentation is the opposite of that. Rather than teach the
// batch compiler a mode nothing here can verify end to end, the coverage run
// keeps the one-shot path it already had.
func newRuntimeCapabilityBatchPoolFor(compiler, prebuiltRuntime string, workers int) *runtimeCapabilityBatchPool {
	if !*runtimeStatusBatchCompile {
		return nil
	}
	if *runtimeCoverageProfile != "" {
		return nil
	}
	return newRuntimeCapabilityBatchPool(compiler, prebuiltRuntime, *runtimeOptimize, workers)
}

// compileRuntimeCapability compiles one program. It takes no *testing.T because
// it runs on its own goroutine, and most of testing.T is not safe to call from
// one that does not own the subtest.
func compileRuntimeCapability(
	compiler string,
	directory string,
	capability runtimeCapability,
) runtimeCapabilityCompilation {
	return compileRuntimeCapabilityWith(compiler, directory, "", capability)
}

// compileRuntimeCapabilityWith compiles one capability program, against a
// prebuilt runtime when the run has one.
//
// The prebuilt runtime is the whole point of the split: the matrix compiles the
// Go runtime once per run instead of once per program. Only the coverage run
// keeps the monolithic path, because instrumenting the runtime is precisely the
// thing a shared prebuilt module cannot do.
func compileRuntimeCapabilityWith(
	compiler string,
	directory string,
	prebuiltRuntime string,
	capability runtimeCapability,
) runtimeCapabilityCompilation {
	source, executable := runtimeCapabilityCompilePaths(directory, capability)

	compileArguments := []string{"-o", executable}
	if *runtimeOptimize {
		compileArguments = append(compileArguments, "-O")
	}
	if prebuiltRuntime != "" {
		compileArguments = append(compileArguments, "-runtime", prebuiltRuntime)
	}
	metadata := ""
	if *runtimeCoverageProfile != "" {
		metadata = executable + ".runtime-cover.json"
		compileArguments = append(compileArguments, "-runtime-covermeta", metadata)
	}
	compileArguments = append(compileArguments, source)

	compile := exec.Command(compiler, compileArguments...)
	started := time.Now()
	output, err := compile.CombinedOutput()

	return runtimeCapabilityCompilation{
		executable: executable,
		metadata:   metadata,
		output:     string(output),
		err:        err,
		duration:   time.Since(started),
		peakRSS:    runtimeCapabilityPeakRSS(compile),
	}
}

func runRuntimeCapabilityProgram(
	t *testing.T,
	compilation runtimeCapabilityCompilation,
	capability runtimeCapability,
) runtimeCapabilityResult {
	t.Helper()

	executable := compilation.executable
	metadata := compilation.metadata
	compileDuration := compilation.duration
	compilePeakRSS := compilation.peakRSS
	defer func() {
		if err := os.Remove(executable); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Logf("remove runtime capability executable: %v", err)
		}
		if metadata != "" {
			os.Remove(metadata)
		}
	}()

	if compilation.err != nil {
		return runtimeCapabilityResult{
			output:          compilation.output,
			err:             errors.New("compile failed: " + compilation.err.Error()),
			compileOutcome:  runtimeCoverageOutcomeFailed,
			runOutcome:      runtimeCoverageOutcomeNotRun,
			coverageOutcome: runtimeCoverageOutcomeNotRun,
			coverageReason:  "compilation failed, so the program was never executed",
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
	coverageOutcome := runtimeCoverageOutcomeNotRequested
	coverageReason := "the run did not request runtime coverage"
	runOutcome := runtimeCoverageOutcomePassed
	var runDuration time.Duration
	var runPeakRSS uint64
	runAttempts := 0
	for attempt := 0; attempt < runCount; attempt++ {
		runAttempts++
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		run := exec.CommandContext(ctx, executable)
		run.Env = append(runtimeCapabilityExecutionEnv(), capability.env...)
		runStarted := time.Now()
		attemptOutput, attemptErr := run.CombinedOutput()
		runDuration += time.Since(runStarted)
		runPeakRSS = max(runPeakRSS, runtimeCapabilityPeakRSS(run))
		if ctx.Err() != nil {
			attemptErr = ctx.Err()
			runOutcome = runtimeCoverageOutcomeTimeout
		} else if attemptErr != nil {
			runOutcome = runtimeCoverageOutcomeFailed
		}
		cancel()

		if metadata != "" {
			var attemptCoverage *runtimeCapabilityCoverageResult
			attemptCoverage, attemptOutput, coverageErr = readRuntimeCapabilityCoverage(metadata, attemptOutput)
			if coverageErr == nil {
				coverageErr = mergeRuntimeCapabilityCoverage(&coverageResult, attemptCoverage)
			}
			if coverageErr != nil {
				coverageOutcome, coverageReason = missingRuntimeCoverageOutcome(capability, runOutcome, timeout, coverageErr)
				if attemptErr == nil {
					attemptErr = coverageErr
				}
			} else {
				coverageOutcome = runtimeCoverageOutcomeCollected
				coverageReason = ""
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
		compileOutcome:  runtimeCoverageOutcomePassed,
		runOutcome:      runOutcome,
		coverageOutcome: coverageOutcome,
		coverageReason:  coverageReason,
		coverageErr:     coverageErr,
		compileDuration: compileDuration,
		compilePeakRSS:  compilePeakRSS,
		runDuration:     runDuration,
		runPeakRSS:      runPeakRSS,
		runAttempts:     runAttempts,
	}
}

// missingRuntimeCoverageOutcome names the absent packet. A program that is
// meant to terminate abnormally is classified rather than reported as a
// collection failure, which is what RUNTIME_PLAN.md section 2 point 4 requires;
// every other absence keeps the failing outcome and says how the process ended.
func missingRuntimeCoverageOutcome(
	capability runtimeCapability,
	runOutcome string,
	timeout time.Duration,
	coverageErr error,
) (string, string) {
	if capability.termination == runtimeCapabilityTerminatesAbnormally {
		reason := capability.terminationNote
		if reason == "" {
			reason = "the program is classified as terminating abnormally"
		}
		return runtimeCoverageOutcomeExpectedUnavailable, reason
	}
	if runOutcome == runtimeCoverageOutcomeTimeout {
		return runtimeCoverageOutcomeMissing, fmt.Sprintf(
			"the process was killed at its %s timeout before the coverage packet was written",
			timeout,
		)
	}
	return runtimeCoverageOutcomeMissing, coverageErr.Error()
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
