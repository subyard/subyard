package ports

// NativeOwnerMember is the one registration member retained across the owning
// compensation rename. Its path is fixed by the original owner recipe.
type NativeOwnerMember struct {
	Device          uint64 `json:"device"`
	Inode           uint64 `json:"inode"`
	UID             uint32 `json:"uid"`
	GID             uint32 `json:"gid"`
	Mode            uint32 `json:"mode"`
	Links           uint64 `json:"links"`
	Size            int64  `json:"size"`
	ModifiedSeconds int64  `json:"modifiedSeconds"`
	ModifiedNanos   int64  `json:"modifiedNanos"`
	ChangedSeconds  int64  `json:"changedSeconds"`
	ChangedNanos    int64  `json:"changedNanos"`
	Digest          string `json:"digest"`
}
