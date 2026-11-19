package collector

// Only these fields may reach producer-owned state. Route credentials and
// connection parameters live in the private operational directory.
var stateMessages = map[string]string{
	"reservation":       "agent_run_id:id boot_id:id manifest_state:reserved launcher_pid:uint launcher_start_ticks:uint",
	"run_manifest":      "agent_run_id:id host_id:id boot_id:id pid:uint start_ticks:uint shared:bool cgroup:label cgroup_identity:*uint scope_root:label scope_root_identity:*uint parent_scope_id:id scope_start_time:time scope_baselines:*scope_baseline",
	"scope_baseline":    "source_time:time event_id:id body:scope_counters",
	"scope_counters":    "agent_run_id:id host_id:id boot_id:id object_id:id generation:id accounting:body:accounting scope_quality:body:scope_quality quality:body:quality shared:bool monotonic_ns:uint observed_process_count:uint populated:bool counter_reset:bool final:bool cpu_ns:?uint cpu_user_ns:?uint cpu_system_ns:?uint memory_bytes:?uint memory_peak_bytes:?uint swap_bytes:?uint io_read_bytes:?uint io_write_bytes:?uint io_read_ops:?uint io_write_ops:?uint",
	"delegation":        "path:label identity:*uint pid:uint start_ticks:uint boot_id:id",
	"broker_intent":     "schema_version:uint agent_run_id:id registered_at:time route_id:hex32 request:broker_request",
	"broker_request":    "kind:body:resource_kind owner_id:id correlation_id:id slot_id:id resource_generation:uint lease_epoch:?uint",
	"broker_observer":   "source_id:id kind:body:resource_kind owner_id:id registration_owner:id route_id:hex32 cursor:cursor registered_at:time associations:@broker_binding polled_at:time last_seen:time offline:bool completed:bool stop_seen:bool",
	"broker_binding":    "agent_run_id:id start_time:time slot_id:id resource_generation:uint lease_epoch:?uint",
	"registration_loss": "reason:body:reason source_time:time event_id:id",
}

var stateRequired = map[string][]string{
	"reservation":       {"agent_run_id", "boot_id", "manifest_state", "launcher_pid", "launcher_start_ticks"},
	"run_manifest":      {"agent_run_id", "host_id", "boot_id", "pid", "start_ticks"},
	"scope_baseline":    {"source_time", "event_id", "body"},
	"scope_counters":    {"object_id"},
	"delegation":        {"path", "identity", "pid", "start_ticks", "boot_id"},
	"broker_intent":     {"schema_version", "agent_run_id", "registered_at", "route_id", "request"},
	"broker_request":    {"kind", "owner_id", "correlation_id", "slot_id", "resource_generation", "lease_epoch"},
	"broker_observer":   {"source_id", "kind", "owner_id", "registration_owner", "route_id", "cursor", "registered_at", "associations"},
	"broker_binding":    {"agent_run_id", "start_time", "slot_id", "resource_generation", "lease_epoch"},
	"registration_loss": {"reason", "source_time", "event_id"},
}
