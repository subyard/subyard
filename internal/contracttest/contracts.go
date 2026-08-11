// Package contracttest contains reusable black-box contracts for production
// and test implementations of control-plane ports.
package contracttest

import (
	"bytes"
	"context"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func ProjectStore(t *testing.T, store ports.ProjectStore) {
	t.Helper()
	ctx := context.Background()
	records, err := store.List(ctx)
	if err != nil || len(records) != 0 {
		t.Fatalf("empty list: records=%#v err=%v", records, err)
	}
	record := ProjectRecord("contract-a")
	if err := store.Put(ctx, record); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.Get(ctx, record.ProjectID)
	if err != nil || got != record {
		t.Fatalf("get: record=%#v err=%v", got, err)
	}
	record.Name = "updated"
	if err := store.Put(ctx, record); err != nil {
		t.Fatalf("replace: %v", err)
	}
	records, err = store.List(ctx)
	if err != nil || len(records) != 1 || records[0].Name != "updated" {
		t.Fatalf("list after replace: records=%#v err=%v", records, err)
	}
	if err := store.Delete(ctx, record.ProjectID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.Delete(ctx, record.ProjectID); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if _, err := store.Get(ctx, record.ProjectID); err == nil {
		t.Fatal("deleted record remained readable")
	}
}

func ProjectRecord(id string) domain.ProjectRecord {
	return domain.ProjectRecord{
		Schema: 1, ProjectID: id, Name: id, HostPath: "/workspace/" + id,
		YardPath: "/srv/workspaces/" + id + "/src", Mode: domain.ProjectSync,
		SSHHost: "yard", ImportedAt: "2026-07-21T00:00:00Z",
	}
}

func IncusRead(t *testing.T, client ports.Incus) {
	t.Helper()
	ctx := context.Background()
	server, err := client.Server(ctx)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if server.Environment != "incus" || server.Version != "6.23" {
		t.Fatalf("unexpected server: %#v", server)
	}
	instance, err := client.Instance(ctx, "subyard", "yard")
	if err != nil {
		t.Fatalf("instance: %v", err)
	}
	if instance.Name != "yard" || instance.Project != "subyard" ||
		instance.Type != domain.YardContainer || instance.Status != "Running" ||
		instance.Config["security.nesting"] != "true" || instance.Devices["root"]["type"] != "disk" {
		t.Fatalf("unexpected instance: %#v", instance)
	}
}

func RemoteTransport(t *testing.T, transport ports.RemoteTransport, target string) {
	t.Helper()
	request := []byte("framed request")
	response, err := transport.Call(context.Background(), target, request)
	if err != nil {
		t.Fatalf("transport call: %v", err)
	}
	if !bytes.Equal(response, request) {
		t.Fatalf("transport changed frame: %q", response)
	}
}
