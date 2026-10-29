package cli

import (
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProfileActivationConcurrentObservationRequiresNativeDeclaredOwner(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeRuntimeProfileFixture(t, root)
	program, err := New(Options{RepositoryRoot: root, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, id string
		inject   func(*Options)
		want     bool
	}{
		{name: "native", id: "sample-runtime", want: true},
		{name: "undeclared", id: "unknown-runtime"},
		{name: "core", id: "core-guest-runtime"},
		{name: "keys", id: "key-tools-runtime"},
		{name: "platform", id: "sample-runtime", inject: func(options *Options) { options.InitPlatform = &initPlatformFixture{} }},
		{name: "incus", id: "sample-runtime", inject: func(options *Options) { options.Incus = &testkit.Incus{} }},
		{name: "executor", id: "sample-runtime", inject: func(options *Options) { options.Executor = &testkit.Incus{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			operation := *program
			if test.inject != nil {
				test.inject(&operation.options)
			}
			observer := operation.profileActivationReconciler(releasetransition.ProcessRequest{}, test.id)
			if got := observer.(releasetransition.V2ConcurrentActivationObserver).ConcurrentActivationObservation(); got != test.want {
				t.Fatalf("concurrent=%t, want %t", got, test.want)
			}
		})
	}
}
