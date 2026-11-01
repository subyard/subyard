package uninstall

import (
	"context"
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"testing"
)

func TestValidateOwnerLockDescriptorKeepsOriginalWithoutUnlock(t *testing.T) {
	home := testkit.TempDir(t)
	_, release, err := OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	held, err := BorrowOwnerLock(home)
	if err != nil || held == nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := ValidateOwnerLockDescriptor(home, uint32(os.Getuid()), held); err != nil {
		t.Fatal(err)
	}
	foreign, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	for _, file := range []*os.File{nil, foreign} {
		if err := ValidateOwnerLockDescriptor(home, uint32(os.Getuid()), file); err == nil {
			t.Fatal("foreign/missing owner admitted")
		}
	}
	if err := ValidateOwnerLockDescriptor(testkit.TempDir(t), uint32(os.Getuid()), held); err == nil {
		t.Fatal("foreign scope admitted")
	}
	if err := ValidateOwnerLockDescriptor(home, uint32(os.Getuid()+1), held); err == nil {
		t.Fatal("foreign UID admitted")
	}
}
