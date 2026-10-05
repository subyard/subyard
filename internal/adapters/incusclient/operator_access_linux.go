package incusclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/operatoraccess"
	"golang.org/x/sys/unix"
)

// DefaultLocalEndpoint excludes endpoints whose service ownership cannot be
// established by the native default-server installer.
func (client *Client) DefaultLocalEndpoint() bool {
	return client.socket == "" && os.Getenv("INCUS_SOCKET") == "" && os.Getenv("INCUS_DIR") == ""
}

// GrantOperatorAccess grants the already approved operator access to the current
// default server socket. It never changes ownership, modes or other ACL entries.
func GrantOperatorAccess(ctx context.Context, uid string) error {
	gid, err := operatoraccess.ApprovedActor(uid)
	if err != nil {
		return err
	}
	if os.Getenv("INCUS_SOCKET") != "" || os.Getenv("INCUS_DIR") != "" || os.Getenv("SUBYARD_INCUS_SOCKET") != "" {
		return errors.New("operator socket access requires the default local Incus endpoint")
	}
	if uid == "0" {
		return nil // Root already has native access; do not add a redundant grant.
	}
	path := "/var/lib/incus/unix.socket"
	if _, err := os.Lstat("/run/incus/unix.socket"); err == nil {
		path = "/run/incus/unix.socket"
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("default Incus socket identity is unavailable")
	}
	return grantSocketAccess(ctx, "/", path, gid, uid, operatoraccess.NativeACL)
}

type socketACLTools = operatoraccess.Tools

func grantSocketAccess(ctx context.Context, rootPath, path string, gid uint32, uid string, tools socketACLTools) error {
	// Pin every component through directory descriptors. No pathname lookup by a
	// privileged ACL tool can select a replacement socket or follow a symlink.
	root, err := unix.Open(rootPath, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	descriptor := root
	defer func() { _ = unix.Close(descriptor) }()
	var rootStat unix.Stat_t
	if err := unix.Fstat(root, &rootStat); err != nil || rootStat.Mode&0022 != 0 {
		return errors.New("default Incus socket root is unsafe")
	}
	path = filepath.Join(rootPath, strings.TrimPrefix(path, "/"))
	components := strings.Split(strings.TrimPrefix(path, rootPath+string(filepath.Separator)), "/")
	if rootPath == "/" {
		components = strings.Split(strings.TrimPrefix(path, "/"), "/")
	}
	ancestors := map[string]unix.Stat_t{rootPath: rootStat}
	currentPath := rootPath
	for index, component := range components {
		flags := unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if index != len(components)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(descriptor, component, flags, 0)
		if err != nil {
			return errors.New("default Incus socket path cannot be pinned")
		}
		_ = unix.Close(descriptor)
		descriptor = next
		var stat unix.Stat_t
		if err := unix.Fstat(descriptor, &stat); err != nil || stat.Uid != rootStat.Uid || index != len(components)-1 && stat.Mode&0022 != 0 {
			return errors.New("default Incus socket path has unsafe ownership or permissions")
		}
		currentPath = filepath.Join(currentPath, component)
		if index != len(components)-1 {
			ancestors[currentPath] = stat
		}
	}
	var before unix.Stat_t
	if err := unix.Fstat(descriptor, &before); err != nil || before.Mode&unix.S_IFMT != unix.S_IFSOCK || before.Mode&07777 != 0660 || before.Gid != gid {
		return errors.New("default Incus socket differs from native ownership and mode")
	}
	file := os.NewFile(uintptr(descriptor), "incus-socket")
	defer file.Close()
	descriptor = -1
	checkIdentity := func() error {
		for path, approved := range ancestors {
			var current unix.Stat_t
			if err := unix.Lstat(path, &current); err != nil || !sameSocketIdentity(approved, current) {
				return errors.New("default Incus socket ancestor changed")
			}
		}
		var current, pinned unix.Stat_t
		if err := unix.Fstat(int(file.Fd()), &pinned); err != nil || !sameSocketIdentity(before, pinned) {
			return errors.New("native Incus socket identity or permissions changed")
		}
		if err := unix.Lstat(path, &current); err != nil || !sameSocketIdentity(before, current) {
			return errors.New("default Incus socket changed while granting operator access")
		}
		return nil
	}
	return operatoraccess.GrantRW(ctx, file, uid, checkIdentity, tools)
}

func sameSocketIdentity(before, after unix.Stat_t) bool {
	return before.Dev == after.Dev && before.Ino == after.Ino && before.Uid == after.Uid && before.Gid == after.Gid && before.Mode == after.Mode
}
