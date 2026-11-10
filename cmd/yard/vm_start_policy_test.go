package main

import "testing"

func TestBootClientPreservesReportingVMConfig(t *testing.T) {
	if !bootIncusClient("").PreserveReportingVMConfig {
		t.Fatal("boot restoration must preserve reporting values for retained rollback readers")
	}
}

func TestVMStartPolicyArguments(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments []string
		inherited string
		socket    string
		refused   bool
	}{
		{name: "inherited socket", arguments: []string{"subyard", "yard"}, inherited: "/run/incus.socket", socket: "/run/incus.socket"},
		{name: "explicit socket", arguments: []string{"subyard", "yard", "/var/lib/incus/unix.socket"}, inherited: "/run/incus.socket", socket: "/var/lib/incus/unix.socket"},
		// Shell passes an empty socket when none is configured.
		{name: "empty socket uses the inherited one", arguments: []string{"subyard", "yard", ""}, inherited: "/run/incus.socket", socket: "/run/incus.socket"},
		{name: "default socket", arguments: []string{"subyard", "yard", ""}},
		{name: "relative socket", arguments: []string{"subyard", "yard", "unix.socket"}, refused: true},
		{name: "relative inherited socket", arguments: []string{"subyard", "yard"}, inherited: "unix.socket", refused: true},
		{name: "missing instance", arguments: []string{"subyard"}, refused: true},
		{name: "extra argument", arguments: []string{"subyard", "yard", "/run/incus.socket", "more"}, refused: true},
		{name: "unsafe project", arguments: []string{"../subyard", "yard"}, refused: true},
		{name: "unsafe instance", arguments: []string{"subyard", "yard;start"}, refused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			project, instance, socket, err := vmStartPolicyArguments(test.arguments, test.inherited)
			if test.refused {
				if err == nil {
					t.Fatalf("accepted %q", test.arguments)
				}
				return
			}
			if err != nil || project != "subyard" || instance != "yard" || socket != test.socket {
				t.Fatalf("parsed %q %q %q, %v; want socket %q", project, instance, socket, err, test.socket)
			}
		})
	}
}
