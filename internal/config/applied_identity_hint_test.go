package config

import (
	"encoding/json"
	"errors"
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"strings"
	"testing"
)

func TestAppliedIdentityHintIsPrivateNonAuthorizingObservation(t *testing.T) {
	root := testkit.TempDir(t)
	hint := AppliedIdentityHint{Schema: 1, Yard: "sample", UUID: "11111111-1111-1111-1111-111111111111", Source: strings.Repeat("a", 64), Release: strings.Repeat("b", 64), User: "dev", UID: 1001}
	if err := PublishAppliedIdentityHint(root, hint, PersistentFileSnapshot{}); err != nil {
		t.Fatal(err)
	}
	actual, before, err := ReadAppliedIdentityHint(root, "sample")
	if err != nil || actual == nil || *actual != hint || before.Identity.Mode&0777 != 0600 {
		t.Fatal(actual, err)
	}
	pending, err := PendingDeveloperIdentities(root)
	if err != nil || len(pending) != 0 {
		t.Fatal("hint acquired execution authority", err)
	}
	other := hint
	other.User = "next"
	if err := PublishAppliedIdentityHint(root, other, PersistentFileSnapshot{}); !errors.Is(err, ErrPersistentTargetStale) {
		t.Fatal("concurrent hint replaced", err)
	}
	if err := PublishAppliedIdentityHint(root, other, before); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"uuid", "source", "release", "unknown", "mode"} {
		t.Run(field, func(t *testing.T) {
			data, _ := json.Marshal(hint)
			var value map[string]any
			json.Unmarshal(data, &value)
			switch field {
			case "uuid":
				value[field] = strings.Repeat("x", 36)
			case "source", "release":
				value[field] = strings.Repeat("x", 64)
			case "unknown":
				value[field] = true
			}
			data, _ = json.Marshal(value)
			testkit.WriteFile(t, AppliedIdentityHintPath(root, "sample"), data, 0600)
			if field == "mode" {
				if err := os.Chmod(AppliedIdentityHintPath(root, "sample"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := ReadAppliedIdentityHint(root, "sample"); err == nil {
				t.Fatal("unsafe hint accepted")
			}
		})
	}
}
