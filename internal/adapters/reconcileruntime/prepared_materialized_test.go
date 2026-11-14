package reconcileruntime

import (
	"fmt"
	"testing"
)

func TestInstalledStageCapacityCountsParentsAndMarker(t *testing.T) {
	p := &PreparedInstalledRuntime{files: map[string][]byte{}}
	for index := 0; index < 8190; index++ {
		p.files[fmt.Sprintf("data/%04d", index)] = []byte("x")
	}
	members, _, err := p.stageUsage()
	if err != nil || members != 8192 {
		t.Fatalf("exact stage capacity: members=%d err=%v", members, err)
	}
	p.files["data/8190"] = []byte("x")
	if _, _, err := p.stageUsage(); err == nil {
		t.Fatal("8191 files plus directory and marker admitted")
	}
}
