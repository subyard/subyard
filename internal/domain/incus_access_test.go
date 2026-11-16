package domain

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestIncusAccessRefusalIsPublicPerYardVerdict(t *testing.T) {
	cause := errors.Join(ErrIncusAccessDenied, os.ErrPermission, errors.New("dial unix /secret/path: permission denied"))
	err := IncusAccessRefusal("alpha", cause)
	if !errors.Is(err, ErrIncusAccessDenied) {
		t.Fatal("refusal must keep the typed cause")
	}
	refusal, ok := err.(interface {
		MigrationRefusal() bool
		ActivationDiagnostic() (string, string)
	})
	if !ok || !refusal.MigrationRefusal() {
		t.Fatal("access denial is a public verdict, not a missing release dependency")
	}
	message, next := refusal.ActivationDiagnostic()
	if message != "yard alpha: native state cannot be inspected: this account cannot access the Incus socket" {
		t.Fatalf("message = %q", message)
	}
	if next != IncusAccessNextStep || !strings.Contains(next, "newgrp incus-admin") || !strings.Contains(next, "usermod -aG incus-admin") || strings.Contains(next, "compatible release") {
		t.Fatalf("next step = %q", next)
	}
	for _, text := range []string{err.Error(), message, next} {
		if strings.Contains(text, "/secret/path") {
			t.Fatalf("raw transport text crossed the boundary: %q", text)
		}
	}
	if IncusAccessRefusal("alpha", nil) != nil {
		t.Fatal("no cause, no refusal")
	}
	if message, _ := IncusAccessDiagnostic("../bad"); !strings.HasPrefix(message, "registered yard: ") {
		t.Fatalf("unsafe yard names stay hidden: %q", message)
	}
}
