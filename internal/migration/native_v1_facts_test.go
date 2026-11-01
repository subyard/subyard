package migration

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// The existing physical fixture's fixed readonly facade must be an ELF
	// image when entered through a retained /proc/self/fd repository. A shell
	// interpreter would reopen that path after CLOEXEC and lose the descriptor.
	if len(os.Args) == 5 && os.Args[1] == "-Y" && os.Args[2] == "test-yard" && os.Args[3] == "test-vms" && os.Args[4] == "status" {
		fmt.Println("ttl_remaining_seconds\t0")
		os.Exit(0)
	}
	os.Exit(m.Run())
}
