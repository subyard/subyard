package ports

// PreparedReadOnlyDisk retains the original instance and local device
// precondition. Owning components choose its fixed resource and destinations.
type PreparedReadOnlyDisk struct {
	Project, Instance, UUID, Device, Source, Destination string
	Before                                               map[string]string
}
