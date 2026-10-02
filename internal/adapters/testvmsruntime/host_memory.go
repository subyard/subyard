package testvmsruntime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const hostMemoryPath = "/var/lib/subyard/host-meminfo"

// CheckHostMemory verifies the live read-only physical-owner source before a
// candidate broker replaces the installed engine. It needs no broker config.
func CheckHostMemory() error {
	_, err := readHostMemoryAvailable(hostMemoryPath)
	return err
}

func readHostMemoryAvailable(path string) (uint64, error) {
	if err := rejectSymlinkPath(path); err != nil {
		return 0, errors.New("physical memory source unavailable or unsafe")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return 0, errors.New("physical memory source unavailable")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil || stat.Type != unix.PROC_SUPER_MAGIC || stat.Flags&unix.ST_RDONLY == 0 {
		return 0, errors.New("physical memory source is not read-only procfs")
	}
	fdinfo, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd))
	if err != nil {
		return 0, errors.New("physical memory descriptor evidence unavailable")
	}
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return 0, errors.New("physical memory mount evidence unavailable")
	}
	if err := validateHostMemoryMount(mountinfo, fdinfo, path); err != nil {
		return 0, err
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return 0, errors.New("physical memory counters unavailable")
	}
	return parseMemAvailable(data)
}

func validateHostMemoryMount(mountinfo, fdinfo []byte, path string) error {
	var mountID string
	for _, line := range strings.Split(string(fdinfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "mnt_id:" {
			continue
		}
		if len(fields) != 2 || mountID != "" {
			return errors.New("physical memory descriptor evidence invalid")
		}
		id, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || id == 0 {
			return errors.New("physical memory descriptor evidence invalid")
		}
		mountID = fields[1]
	}
	if mountID == "" {
		return errors.New("physical memory descriptor evidence unavailable")
	}
	found := false
	for _, line := range strings.Split(string(mountinfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[0] != mountID {
			continue
		}
		separator := 6
		for separator < len(fields) && fields[separator] != "-" {
			separator++
		}
		if found || separator+3 >= len(fields) || fields[3] != "/meminfo" || fields[4] != path || fields[separator+1] != "proc" ||
			!strings.Contains(","+fields[5]+",", ",ro,") || strings.Contains(","+fields[5]+",", ",rw,") {
			return errors.New("physical memory mount is not an exact read-only meminfo bind")
		}
		found = true
	}
	if !found {
		return errors.New("physical memory mount evidence unavailable")
	}
	return nil
}

func parseMemAvailable(data []byte) (uint64, error) {
	var available uint64
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "MemAvailable:" {
			continue
		}
		if found || len(fields) != 3 || fields[2] != "kB" {
			return 0, errors.New("physical available memory invalid")
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || value > ^uint64(0)/1024 {
			return 0, errors.New("physical available memory invalid")
		}
		available, found = value*1024, true
	}
	if !found {
		return 0, errors.New("physical available memory unavailable")
	}
	return available, nil
}

func capPhysicalMemory(visible MemoryCapacity, available uint64) MemoryCapacity {
	visible.Available = min(visible.Available, available)
	visible.PhysicalAvailable = available
	visible.PhysicalAvailableKnown = true
	return visible
}
