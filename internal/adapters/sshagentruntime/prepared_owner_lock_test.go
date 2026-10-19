package sshagentruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func ownerLockFixture(t *testing.T) (*PreparedLock, Manager) {
	t.Helper()
	identity := sha256.Sum256([]byte("original control identity"))
	directory := filepath.Join(testkit.TempDir(t), "grant-\xff")
	p := &PreparedLock{directory: directory, identity: hex.EncodeToString(identity[:]), state: "granted", expires: time.Date(2031, 2, 3, 4, 5, 6, 123456789, time.FixedZone("original offset", 27000))}
	return p, Manager{Config: Config{Directory: directory}}
}

func TestPreparedOwnerLockRecordPreservesOriginalFieldsAndGuard(t *testing.T) {
	original, manager := ownerLockFixture(t)
	for _, state := range []string{"locked", "pending", "granted"} {
		for _, expires := range []time.Time{{}, original.expires} {
			t.Run(state+"/"+expires.Format(time.RFC3339Nano), func(t *testing.T) {
				p := *original
				p.state, p.expires = state, expires
				raw, err := p.CaptureRecord()
				if err != nil {
					t.Fatal(err)
				}
				restored, err := manager.RestorePreparedLockRecord(raw, p.Binding(), p.State())
				if err != nil {
					t.Fatal(err)
				}
				if restored == &p || restored.directory != p.directory || restored.identity != p.identity || restored.State() != p.State() || restored.Binding() != p.Binding() || !restored.expires.Equal(p.expires) {
					t.Fatal("original prepared grant changed")
				}
				before, _ := json.Marshal(p.expires)
				after, _ := json.Marshal(restored.expires)
				if !bytes.Equal(before, after) {
					t.Fatal("original expiry serialization changed")
				}
				// Restoration cannot select the current replacement grant. The
				// original guard still refuses it and permits locked convergence.
				replacement := p
				replacement.state = "granted"
				replacement.identity = strings.Repeat("0", 64)
				if err := restored.Check(&replacement); !errors.Is(err, domain.ErrPlanStale) {
					t.Fatal("restored identity accepted a replacement grant", err)
				}
				replacement.state = "locked"
				if err := restored.Check(&replacement); err != nil {
					t.Fatal("locked convergence changed", err)
				}
				// Retained fields do not alias either the wire buffer or a later
				// mutation of the original preparation.
				binding := restored.Binding()
				for index := range raw {
					raw[index] = 0
				}
				p.directory, p.identity, p.state, p.expires = "changed", "changed", "locked", time.Time{}
				if restored.Binding() != binding {
					t.Fatal("restoration aliases original storage")
				}
			})
		}
	}
}

func TestPreparedOwnerLockRecordFreshProcessWithoutObservation(t *testing.T) {
	if os.Getenv("SUBYARD_OWNER_LOCK_RECORD_CHILD") == "1" {
		args := os.Args[len(os.Args)-4:]
		raw, err := os.ReadFile(args[0])
		if err != nil {
			t.Fatal(err)
		}
		manager := Manager{Config: Config{Directory: args[1]}}
		p, err := manager.RestorePreparedLockRecord(raw, args[2], args[3])
		if err != nil || p == nil {
			t.Fatal("restore original grant in fresh process", err)
		}
		recaptured, err := p.CaptureRecord()
		if err != nil || !bytes.Equal(raw, recaptured) || p.Binding() != args[2] || p.State() != args[3] {
			t.Fatal("fresh process changed original preparation", err)
		}
		return
	}
	p, manager := ownerLockFixture(t)
	raw, err := p.CaptureRecord()
	if err != nil {
		t.Fatal(err)
	}
	// Current control state deliberately cannot be observed as a grant. The
	// original record must still restore without reading or changing this file.
	sentinel := []byte("current effect drift must not become a new baseline")
	testkit.WriteFile(t, manager.Config.Directory, sentinel, 0o600)
	if _, err := manager.Status(context.Background()); err == nil {
		t.Fatal("fixture unexpectedly permits a live status observation")
	}
	recordPath := filepath.Join(testkit.TempDir(t), "original-lock.json")
	testkit.WriteFile(t, recordPath, raw, 0o600)
	command := exec.Command(os.Args[0], "-test.run=^TestPreparedOwnerLockRecordFreshProcessWithoutObservation$", "--", recordPath, p.directory, p.Binding(), p.State())
	command.Env = append(os.Environ(), "SUBYARD_OWNER_LOCK_RECORD_CHILD=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fresh process: %v\n%s", err, output)
	}
	current, err := os.ReadFile(manager.Config.Directory)
	if err != nil || !bytes.Equal(current, sentinel) {
		t.Fatal("restoration changed current control state", err)
	}
	entries, err := os.ReadDir(filepath.Dir(manager.Config.Directory))
	if err != nil || len(entries) != 1 {
		t.Fatal("restoration created control or lock files", err)
	}
}

func TestPreparedOwnerLockRecordRejectsMalformedAndChangedOriginal(t *testing.T) {
	p, manager := ownerLockFixture(t)
	raw, err := p.CaptureRecord()
	if err != nil {
		t.Fatal(err)
	}
	var record preparedOwnerLockRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	alter := func(change func(*preparedOwnerLockRecord)) []byte {
		copy := record
		change(&copy)
		result, err := json.Marshal(copy)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	cases := map[string][]byte{
		"truncated":               raw[:len(raw)-1],
		"unknown":                 append([]byte(`{"Unknown":true,`), raw[1:]...),
		"duplicate":               append([]byte(`{"Version":1,`), raw[1:]...),
		"trailing-value":          append(append([]byte{}, raw...), []byte(`{}`)...),
		"noncanonical-space":      append(append([]byte{}, raw...), '\n'),
		"noncanonical-field-case": bytes.Replace(raw, []byte(`"Version"`), []byte(`"version"`), 1),
		"null":                    []byte("null"),
		"oversized":               bytes.Repeat([]byte("x"), preparedOwnerLockRecordLimit+1),
		"version":                 alter(func(r *preparedOwnerLockRecord) { r.Version++ }),
		"directory":               alter(func(r *preparedOwnerLockRecord) { r.Directory = []byte("another directory") }),
		"identity":                alter(func(r *preparedOwnerLockRecord) { r.Identity = bytes.Repeat([]byte("0"), 64) }),
		"invalid-digest":          alter(func(r *preparedOwnerLockRecord) { r.Identity = []byte("not a digest") }),
		"state":                   alter(func(r *preparedOwnerLockRecord) { r.State = []byte("pending") }),
		"invalid-state":           alter(func(r *preparedOwnerLockRecord) { r.State = []byte("unlocked") }),
		"expiry":                  alter(func(r *preparedOwnerLockRecord) { r.Expires = []byte(`"2031-02-03T04:05:07.123456789+07:30"`) }),
		"invalid-expiry":          alter(func(r *preparedOwnerLockRecord) { r.Expires = []byte(`"tomorrow"`) }),
		"null-expiry":             alter(func(r *preparedOwnerLockRecord) { r.Expires = []byte(`null`) }),
		"noncanonical-expiry":     alter(func(r *preparedOwnerLockRecord) { r.Expires = []byte(`"2031-02-03T04:05:06.1234567890+07:30"`) }),
	}
	for name, candidate := range cases {
		t.Run(name, func(t *testing.T) {
			if restored, err := manager.RestorePreparedLockRecord(candidate, p.Binding(), p.State()); !errors.Is(err, domain.ErrPlanStale) || restored != nil {
				t.Fatal("changed original record accepted", err)
			}
		})
	}
	for _, expected := range []struct{ binding, state string }{
		{strings.Repeat("0", 64), p.State()}, {"bad digest", p.State()}, {strings.ToUpper(p.Binding()), p.State()}, {p.Binding(), "pending"}, {p.Binding(), "unlocked"}, {"", ""},
	} {
		if restored, err := manager.RestorePreparedLockRecord(raw, expected.binding, expected.state); !errors.Is(err, domain.ErrPlanStale) || restored != nil {
			t.Fatal("wrong original binding or state accepted", err)
		}
	}
	manager.Config.Directory += "-other"
	if restored, err := manager.RestorePreparedLockRecord(raw, p.Binding(), p.State()); !errors.Is(err, domain.ErrPlanStale) || restored != nil {
		t.Fatal("another manager directory accepted", err)
	}
}

func TestPreparedOwnerLockRecordAbsentAndInvalidCapture(t *testing.T) {
	var absent *PreparedLock
	raw, err := absent.CaptureRecord()
	if err != nil || raw != nil {
		t.Fatal("nil preparation did not stay absent", err)
	}
	p, manager := ownerLockFixture(t)
	if restored, err := manager.RestorePreparedLockRecord(raw, "", ""); err != nil || restored != nil {
		t.Fatal("absent record fabricated a prepared grant", err)
	}
	for _, raw := range [][]byte{nil, {}} {
		if restored, err := manager.RestorePreparedLockRecord(raw, p.Binding(), p.State()); !errors.Is(err, domain.ErrPlanStale) || restored != nil {
			t.Fatal("missing original preparation accepted", err)
		}
	}
	for _, change := range []func(*PreparedLock){
		func(p *PreparedLock) { p.identity = "bad digest" },
		func(p *PreparedLock) { p.state = "unlocked" },
		func(p *PreparedLock) { p.directory = "" },
		func(p *PreparedLock) { p.directory = strings.Repeat("x", preparedOwnerLockRecordLimit) },
		func(p *PreparedLock) { p.expires = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
	} {
		copy := *p
		change(&copy)
		if raw, err := copy.CaptureRecord(); err == nil || raw != nil {
			t.Fatal("invalid preparation captured")
		}
	}
}
