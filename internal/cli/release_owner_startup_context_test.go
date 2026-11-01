package cli

import (
	"context"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type startupContextHost struct {
	*startupRecordHost
	contexts []domain.Context
}

func (h *startupContextHost) InspectNetwork(ctx context.Context, yards []yardnetwork.Yard) (yardnetwork.Snapshot, error) {
	snapshot, err := h.startupRecordHost.InspectNetwork(ctx, yards)
	if err != nil {
		return snapshot, err
	}
	for _, c := range h.contexts {
		y := networkYard(c)
		if y != networkYard(h.loaded.Context) {
			snapshot.Yards = append(snapshot.Yards, yardnetwork.ObservedYard{Yard: y})
		}
	}
	return snapshot, nil
}
func TestOwnerStartupRetainedHandlerUsesOriginalPeerConfiguration(t *testing.T) {
	for _, fault := range []string{"original", "settings", "source"} {
		t.Run(fault, func(t *testing.T) {
			program, _, _ := orphanIngressFixture(t, true)
			root := program.options.RepositoryRoot
			descriptorPath := filepath.Join(root, "config/profiles/sample/resources/relay.res")
			descriptor, err := os.ReadFile(descriptorPath)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, descriptorPath, append(descriptor, []byte("STARTUP=bringup\n")...), 0o600)
			if err := os.MkdirAll(filepath.Join(root, "state/yards/sibling"), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "state/yards/sibling/config.env"), []byte("YARD_KIND=container\n"), 0o600)
			testkit.WriteFile(t, filepath.Join(root, "config/profiles/sample/resources/relay/handler.sh"), []byte(originalStartupProcessHandler), 0o700)
			cli, loaded, base := originalStartupProcessCLI(t, root, program.options.Environment)
			if err := os.MkdirAll(loaded.Context.Paths.DataHome, 0o700); err != nil {
				t.Fatal(err)
			}
			contexts, err := cli.powerYardContexts(loaded)
			if err != nil {
				t.Fatal(err)
			}
			found := map[string]bool{}
			for _, c := range contexts {
				found[c.YardName] = true
			}
			if !found["default"] || !found["sibling"] {
				t.Fatal("fixture omitted default/sibling")
			}
			host := &startupContextHost{startupRecordHost: base, contexts: contexts}
			cli.options.NetworkPolicy = &yardnetwork.Service{Host: host, Lock: testNetworkPolicyLock{}, ClearStaleUDP: func(context.Context, netip.AddrPort) error { return nil }}
			original, err := cli.prepareResourceStartup(context.Background(), loaded)
			if err != nil || original == nil {
				t.Fatal("original prepare", err)
			}
			original.intent.instanceUUID = "original-startup-uuid"
			raw, err := captureOwnerResourceStartup(original, cli, root)
			if err != nil {
				t.Fatal(err)
			}
			retained := testkit.TempDir(t)
			if err := filepath.WalkDir(filepath.Join(root, "config"), func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				relative, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				target := filepath.Join(retained, relative)
				if d.IsDir() {
					return os.MkdirAll(target, 0o700)
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				testkit.WriteFile(t, target, body, info.Mode().Perm())
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := reconcileruntime.Runtime{RepositoryRoot: root, Environment: program.options.Environment}
			after := before
			after.RepositoryRoot = retained
			physical, err := ownerResourceSourceCLI(cli, before, after)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "source" {
				testkit.WriteFile(t, filepath.Join(retained, "config/profiles/sample/resources/relay/handler.sh"), []byte("#!/bin/sh\nexit 77\n"), 0o700)
			}
			startup, err := restoreOwnerResourceStartup(raw, physical, loaded, root)
			if fault == "source" {
				if err == nil {
					t.Fatal("changed retained handler restored")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// Resolve both default and sibling through the actual original config guard.
			for _, c := range contexts {
				if _, err := startup.ingress.service.ContractSource(networkYard(c)); err != nil {
					t.Fatal("original peer config rejected", c.YardName, err)
				}
			}
			if fault == "settings" {
				testkit.WriteFile(t, filepath.Join(root, "state/yards/sibling/config.env"), []byte("YARD_KIND=invalid-kind\n"), 0o600)
			}
			_, release, err := uninstall.OwnerLock(context.Background(), loaded.Context.Paths.DataHome, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			observer := &startupProcessObserver{root: loaded.Context.Paths.DataHome}
			ctx := ports.WithNativeOwnerHome(ports.WithNativeOperationObserver(context.Background(), observer), loaded.Context.Paths.DataHome)
			err = startup.apply(ctx, physical, "original-startup-peers")
			if fault == "settings" {
				if err == nil || observer.commands != 0 {
					t.Fatal("changed peer settings reached effect", err, observer.commands)
				}
				return
			}
			if err != nil || observer.commands != 1 {
				t.Fatal("actual startup.apply rejected original peers", err, observer.commands)
			}
			if _, err := os.Stat(filepath.Join(loaded.Context.Paths.DataHome, "committed")); err != nil {
				t.Fatal("original intent not committed", err)
			}
		})
	}
}
