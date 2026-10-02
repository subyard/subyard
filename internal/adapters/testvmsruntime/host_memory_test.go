package testvmsruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestParseMemAvailableRejectsAmbiguousMemoryAndIgnoresSwap(t *testing.T) {
	for _, test := range []struct {
		name, body string
		available  uint64
		invalid    bool
	}{
		{"memory only", "MemAvailable: 123 kB\n", 123 * 1024, false},
		{"swap ignored", "MemAvailable: 123 kB\nSwapTotal: 999999999 kB\nSwapFree: 999999999 kB\n", 123 * 1024, false},
		{"exhausted", "MemAvailable: 0 kB\n", 0, false},
		{"largest safe value", "MemAvailable: 18014398509481983 kB\n", 18014398509481983 * 1024, false},
		{"missing", "SwapFree: 999999999 kB\n", 0, true},
		{"invalid number", "MemAvailable: unknown kB\n", 0, true},
		{"negative", "MemAvailable: -1 kB\n", 0, true},
		{"wrong unit", "MemAvailable: 123 MB\n", 0, true},
		{"missing unit", "MemAvailable: 123\n", 0, true},
		{"extra field", "MemAvailable: 123 kB extra\n", 0, true},
		{"duplicate", "MemAvailable: 123 kB\nMemAvailable: 456 kB\n", 0, true},
		{"malformed duplicate", "MemAvailable: 123 kB\nMemAvailable: unknown kB\n", 0, true},
		{"conversion overflow", "MemAvailable: 18014398509481984 kB\n", 0, true},
		{"integer overflow", "MemAvailable: 18446744073709551616 kB\n", 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			available, err := parseMemAvailable([]byte(test.body))
			if test.invalid {
				if err == nil {
					t.Fatal("accepted invalid memory telemetry")
				}
			} else if err != nil || available != test.available {
				t.Fatalf("available = %d, error = %v; want %d", available, err, test.available)
			}
		})
	}
}

func TestHostMemoryMountRequiresExactReadOnlyProcFile(t *testing.T) {
	valid := "91 40 0:5 /meminfo " + hostMemoryPath + " ro,nosuid,nodev,noexec,relatime shared:1 - proc proc rw\n"
	fdinfo := "pos:\t0\nflags:\t0100000\nmnt_id:\t91\nino:\t12345\n"
	for _, test := range []struct {
		name, mountinfo, fdinfo string
		valid                   bool
	}{
		{"readonly bind with writable superblock", valid, fdinfo, true},
		{"no optional fields", strings.Replace(valid, " shared:1", "", 1), fdinfo, true},
		{"missing mount", "", fdinfo, false},
		{"writable", strings.Replace(valid, " ro,", " rw,", 1), fdinfo, false},
		{"readonly substring", strings.Replace(valid, " ro,", " errors=remount-ro,", 1), fdinfo, false},
		{"wrong root", strings.Replace(valid, " /meminfo ", " /swaps ", 1), fdinfo, false},
		{"wrong target", strings.Replace(valid, hostMemoryPath, hostMemoryPath+"-other", 1), fdinfo, false},
		{"LXCFS", strings.Replace(valid, "- proc proc", "- fuse.lxcfs lxcfs", 1), fdinfo, false},
		{"snapshot filesystem", strings.Replace(valid, "- proc proc", "- ext4 /dev/test", 1), fdinfo, false},
		{"mismatched descriptor", valid, strings.Replace(fdinfo, "91", "92", 1), false},
		{"missing descriptor mount", valid, "pos: 0\n", false},
		{"malformed descriptor mount", valid, "mnt_id: unknown\n", false},
		{"duplicate descriptor mount", valid, fdinfo + "mnt_id: 92\n", false},
		{"malformed mount", "91 40 0:5 /meminfo " + hostMemoryPath + " ro\n", fdinfo, false},
		{"duplicate mount", valid + valid, fdinfo, false},
		{"other mount does not replace pinned descriptor", valid + strings.Replace(valid, "91 40", "92 91", 1), fdinfo, true},
		{"shadowed unsafe mount", valid + strings.Replace(strings.Replace(valid, "91 40", "92 91", 1), " /meminfo ", " /swaps ", 1), strings.Replace(fdinfo, "91", "92", 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateHostMemoryMount([]byte(test.mountinfo), []byte(test.fdinfo), hostMemoryPath)
			if (err == nil) != test.valid {
				t.Fatalf("mount validation = %v; valid = %t", err, test.valid)
			}
		})
	}
}

func TestReadHostMemoryAvailableRejectsSnapshotsAndSymlinks(t *testing.T) {
	root := testkit.TempDir(t)
	snapshot := filepath.Join(root, "snapshot")
	testkit.WriteFile(t, snapshot, []byte("MemAvailable: 999999999 kB\n"), 0o400)
	link := filepath.Join(root, "link")
	if err := os.Symlink(snapshot, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{snapshot, link, filepath.Join(root, "missing")} {
		if _, err := readHostMemoryAvailable(path); err == nil {
			t.Fatal("accepted an unverified physical memory source")
		}
	}
}

func TestPhysicalMemoryCapacityPreservesVisibleBoundary(t *testing.T) {
	proc, group, write := memoryFixture(t)
	write(filepath.Join(group, "memory.peak"), "3145728\n")
	write(filepath.Join(group, "memory.events"), "oom 2\noom_kill 1\n")
	visible, err := memoryCapacity(proc, group)
	if err != nil {
		t.Fatal(err)
	}
	for _, physical := range []uint64{0, 1 << 20, 8 << 20} {
		got := capPhysicalMemory(visible, physical)
		want := visible
		want.Available = min(visible.Available, physical)
		want.PhysicalAvailable, want.PhysicalAvailableKnown = physical, true
		if got != want {
			t.Fatalf("physical cap changed visible boundary evidence: got %+v, want %+v", got, want)
		}
	}
}

func TestPhysicalMemoryCapacityUsesLowerHeadroom(t *testing.T) {
	for _, test := range []struct {
		name                         string
		visible, physical, available uint64
	}{
		{"LXCFS above physical", 32 << 30, 14 << 30, 14 << 30},
		{"visible memory below physical", 10 << 30, 20 << 30, 10 << 30},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := capPhysicalMemory(MemoryCapacity{Available: test.visible}, test.physical)
			if got.Available != test.available || !got.PhysicalAvailableKnown || got.PhysicalAvailable != test.physical {
				t.Fatalf("incorrect lower memory bound: %+v", got)
			}
		})
	}
}
