package reconcileruntime

import (
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestInitReexecPreservesExplicitProfile(t *testing.T) {
	runtime := Runtime{Yard: domain.Context{YardName: "vpn"}, InitProfile: "sample"}
	args, err := runtime.initReexecArguments("/opt/yard")
	want := []string{"/opt/yard", "-Y", "vpn", "init", "--yes", "--profile", "sample"}
	if err != nil || !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%v err=%v", args, err)
	}
	runtime.InitProfile = ""
	args, err = runtime.initReexecArguments("/opt/yard")
	if err != nil || !reflect.DeepEqual(args, want[:5]) {
		t.Fatalf("plain init args=%v err=%v", args, err)
	}
	runtime.InitProfile = "../sample"
	if _, err := runtime.initReexecArguments("/opt/yard"); err == nil {
		t.Fatal("unsafe profile accepted")
	}
}
