package config

// Test VM admission defaults are resolved by the engine. Physical shell
// adapters receive these values through the prepared environment.
const (
	DefaultTestVMDiskBudget    = "0GiB"
	DefaultTestVMCacheBudget   = "24GiB"
	DefaultTestVMDiskReserve   = "5GiB"
	DefaultTestVMMemoryReserve = "4GiB"
	DefaultTestVMOverhead      = "512MiB"
)
