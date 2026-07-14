package bpf

// helpers maps eBPF helper-function names to their kernel BPF_FUNC_* numbers, so
// a C program can call them by name. This is a common subset; unknown names are
// rejected at compile time. (BPF-to-BPF calls between compiled functions are not
// yet supported.)
var helpers = map[string]int32{
	"bpf_map_lookup_elem":       1,
	"bpf_map_update_elem":       2,
	"bpf_map_delete_elem":       3,
	"bpf_probe_read":            4,
	"bpf_ktime_get_ns":          5,
	"bpf_trace_printk":          6,
	"bpf_get_prandom_u32":       7,
	"bpf_get_smp_processor_id":  8,
	"bpf_tail_call":             12,
	"bpf_get_current_pid_tgid":  14,
	"bpf_get_current_uid_gid":   15,
	"bpf_get_current_comm":      16,
	"bpf_perf_event_output":     25,
	"bpf_get_current_task":      35,
	"bpf_get_current_cgroup_id": 80,
	"bpf_probe_read_user":       112,
	"bpf_probe_read_kernel":     113,
	"bpf_ktime_get_boot_ns":     125,
	"bpf_get_func_ip":           157,
}

// helperID returns the BPF_FUNC number for a helper name.
func helperID(name string) (int32, bool) {
	id, ok := helpers[name]
	return id, ok
}
