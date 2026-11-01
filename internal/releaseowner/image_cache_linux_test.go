package releaseowner

import (
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
)

func TestOwnerImageCachePendingProvenanceAndMissingAckNeverTerminal(t *testing.T) {
	o, _ := ownerCreateFixture(t)
	before := ports.OwnerImageSourceGuard{Path: []byte("/original/config/oidctokens/source.json"), State: "file:" + strings.Repeat("a", 64)}
	after := before
	after.State = "file:" + strings.Repeat("b", 64)
	first := ports.OwnerImageCacheSuccessor{Policy: strings.Repeat("c", 64), Before: []ports.OwnerImageSourceGuard{before}, After: []ports.OwnerImageSourceGuard{after}}
	var actual []ports.OwnerImageCacheSuccessor
	if input := os.Getenv("SUBYARD_NATIVE_IMAGE_ENDPOINT_FACTS"); input != "" {
		raw, err := os.ReadFile(input)
		if err != nil || json.Unmarshal(raw, &actual) != nil || len(actual) != 2 {
			t.Fatal("actual SDK endpoint facts unavailable", err)
		}
		first = actual[0]
		before = first.Before[0]
		after = first.After[0]
	}

	if err := o.RecordOwnerImageCache(first); err != nil {
		t.Fatal(err)
	}
	// A completely fresh observer can read this exact original pending endpoint,
	// yet query still has no outer terminal/result/Ready or replay authority.
	fresh := &invocationObserver{session: o.session, binding: o.binding}
	facts, err := fresh.ReadOwnerImageCache(first.Policy)
	if err != nil || !reflect.DeepEqual(facts, []ports.OwnerImageCacheSuccessor{first}) {
		t.Fatal("original pending cache lost", err)
	}
	second := first
	second.Before = []ports.OwnerImageSourceGuard{after}
	next := after
	next.State = "file:" + strings.Repeat("d", 64)
	second.After = []ports.OwnerImageSourceGuard{next}
	if len(actual) > 0 {
		second = actual[1]
	}
	if err := fresh.RecordOwnerImageCache(second); err != nil {
		t.Fatal("fresh cleanup cache sequence reset", err)
	}
	facts, err = fresh.ReadOwnerImageCache(first.Policy)
	if err != nil || len(facts) != 2 {
		t.Fatal(err, len(facts))
	}
	if _, err := o.session.readTerminal(o.binding); err == nil {
		t.Fatal("cache provenance manufactured terminal")
	}
	if output := os.Getenv("SUBYARD_NATIVE_IMAGE_CACHE_CORPUS"); output != "" {
		fd, err := o.session.factsDirectory(o.binding)
		if err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join("/proc/self/fd", fmt.Sprint(fd))
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		files := map[string][]byte{}
		for _, entry := range entries {
			raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			files[entry.Name()] = raw
		}
		unix.Close(fd)
		raw, err := json.Marshal(struct {
			Binding Binding
			Files   map[string][]byte
		}{o.binding, files})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(output, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(output, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	foreign := &invocationObserver{session: fresh.session, binding: fresh.binding}
	foreign.binding.Ordinal++
	if _, err := foreign.ReadOwnerImageCache(first.Policy); err == nil {
		t.Fatal("foreign invocation adopted cache endpoint")
	}
	directory, err := o.session.factsDirectory(o.binding)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join("/proc/self/fd", fmt.Sprint(directory), factName(o.binding.Ordinal, "image-cache-001"))
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-2] = 'x'
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	unix.Close(directory)
	if _, err := fresh.ReadOwnerImageCache(first.Policy); err == nil {
		t.Fatal("tampered cache fact accepted")
	}
}
