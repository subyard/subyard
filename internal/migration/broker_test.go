package migration

import (
	"context"
	"strings"
	"testing"
)

func TestBrokerRollbackRunnerPolicy(t *testing.T) {
	tests := []struct {
		name       string
		fromLayout int
		want       brokerRollbackDisposition
		wantError  bool
	}{
		{name: "layout 1 catch-up", fromLayout: 1, want: brokerRollbackDeferredToOwner},
		{name: "direct layout 2 rollback", fromLayout: 2, want: brokerRollbackWithPrevious},
		{name: "current layout rollback", fromLayout: 5, want: brokerRollbackWithPrevious},
		{name: "invalid layout", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := brokerRollbackPolicy(test.fromLayout)
			if (err != nil) != test.wantError {
				t.Fatalf("runner policy error = %v, want error %v", err, test.wantError)
			}
			if err == nil && got != test.want {
				t.Fatalf("broker rollback disposition = %v, want %v", got, test.want)
			}
		})
	}
}

func TestLayoutOneLegacyDesiredPowerUsesPreparedBrokerState(t *testing.T) {
	tests := []struct {
		name       string
		fromLayout int
		before     string
		want       string
		wantError  bool
	}{
		{name: "active", fromLayout: 1, before: "active", want: "running"},
		{name: "inactive", fromLayout: 1, before: "inactive", want: "stopped"},
		{name: "absent", fromLayout: 1, before: "absent"},
		{name: "unprepared", fromLayout: 1},
		{name: "later layout", fromLayout: 2, before: "active"},
		{name: "invalid", fromLayout: 1, before: "paused", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tx := transaction{
				FromLayout: test.fromLayout,
				Operations: []transactionOperation{{
					Kind:   OperationKindTestVMBrokerRuntimeV1,
					Before: test.before,
				}},
			}
			got, err := layoutOneLegacyDesiredPower(tx)
			if (err != nil) != test.wantError {
				t.Fatalf("legacy desired power error = %v, want error %v", err, test.wantError)
			}
			if err == nil && got != test.want {
				t.Fatalf("legacy desired power = %q, want %q", got, test.want)
			}
		})
	}
}

func TestLayoutOneBrokerRollbackDefersWithoutCallingRuntime(t *testing.T) {
	err := rollbackTypedOperation(
		context.Background(),
		ReleaseOptions{},
		Operation{ID: "test-vm-broker-runtime", Kind: OperationKindTestVMBrokerRuntimeV1},
		"active",
		1,
	)
	if err != nil {
		t.Fatalf("layout-1 broker rollback was not deferred to owner: %v", err)
	}

	err = rollbackTypedOperation(
		context.Background(),
		ReleaseOptions{},
		Operation{ID: "test-vm-broker-runtime", Kind: OperationKindTestVMBrokerRuntimeV1},
		"active",
		2,
	)
	if err == nil || !strings.Contains(err.Error(), "executable is required") {
		t.Fatalf("layout-2 broker rollback did not call retained runtime: %v", err)
	}
}
