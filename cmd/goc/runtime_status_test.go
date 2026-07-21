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
			expectation: runtimeCapabilityMustPass,
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
			category:    "stdlib-text",
			name:        "strconv-roundtrip",
			source:      "stdlib_strconv_roundtrip.go",
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
			category:    "stdlib-sync",
			name:        "once-map-cond",
			source:      "stdlib_sync_once_map_cond.go",
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
			name:        "trace-buffer",
			source:      "stdlib_runtime_trace_buffer.go",
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

	for _, capability := range capabilities {
		capability := capability
		t.Run(capability.category+"/"+capability.name, func(t *testing.T) {
			if capability.requiresAFINET && !afinetSocketAvailable {
				t.Skipf("AF_INET sockets unavailable in this execution environment: %v", afinetSocketErr)
			}
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
