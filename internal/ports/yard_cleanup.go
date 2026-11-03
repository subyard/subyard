package ports

import "context"

// YardCleanupBackup binds a stopped yard's independent native volume copy.
// The backup lives in the default Incus project, outside selected-yard teardown.
type YardCleanupBackup struct {
	Project, Instance, Pool, Volume, OriginalUUID, Token   string
	BackupVolume, RestoreVolume, ContentType, DesiredPower string
	Native                                                 []byte
}

type YardBackupManager interface {
	PrepareYardBackup(context.Context, string, string, string, string, string, map[string]string) (YardCleanupBackup, error)
	CheckYardBackupSource(context.Context, YardCleanupBackup) error
	CopyYardBackup(context.Context, YardCleanupBackup) error
	CheckYardBackup(context.Context, YardCleanupBackup) error
	AttachYardBackup(context.Context, YardCleanupBackup, string) error
	DetachYardBackup(context.Context, YardCleanupBackup, string) error
	ReleaseYardBackup(context.Context, YardCleanupBackup) error
	RestoreYardBinds(context.Context, YardCleanupBackup, string) error
}
