package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// runInstallation is a private installer leaf. Its caller owns package
// verification and consent; this boundary only publishes exact scoped metadata.
func (cli *CLI) runInstallation(ctx context.Context, arguments []string) int {
	if len(arguments) != 1 || (arguments[0] != "runtime" && arguments[0] != "full" && arguments[0] != "inspect") {
		return 2
	}
	var request struct {
		BinDir      string
		ShellFiles  []string
		CacheInputs []string
	}
	decoder := json.NewDecoder(io.LimitReader(cli.options.Stdin, 65537))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		cli.errorf("invalid installation publication request")
		return 2
	}
	includeBootstrap := cli.env["SUBYARD_INSTALL_RUNTIME_SCOPE"] != "private-draft"
	identity, err := cli.installationIdentity("", "", includeBootstrap)
	if err != nil {
		cli.errorf("installation: %v", err)
		return 1
	}
	if arguments[0] == "inspect" {
		directories := []string{identity.RuntimeRoot}
		if includeBootstrap {
			directories = append(directories, identity.DataHome)
		}
		for _, directory := range directories {
			if _, err := os.Lstat(filepath.Join(directory, uninstall.ManifestName)); os.IsNotExist(err) {
				continue
			}
			before, err := uninstall.InspectInstallation(directory, identity)
			if err != nil {
				cli.errorf("installation: %v", err)
				return 1
			}
			value := before.Installation
			before.Close()
			if directory == identity.DataHome && request.BinDir != "" && value.BinDir != request.BinDir {
				cli.errorf("installation entrypoint ownership conflicts")
				return 1
			}
		}
		identity.BinDir, identity.ShellFiles = request.BinDir, request.ShellFiles
		if err := identity.Validate(identity.DataHome, identity.OperatorUID); err != nil {
			cli.errorf("installation: %v", err)
			return 1
		}
		return 0
	}
	directory := identity.RuntimeRoot
	if arguments[0] == "full" {
		directory = identity.DataHome
	}
	before, err := uninstall.InspectInstallation(directory, identity)
	if err != nil {
		cli.errorf("installation: %v", err)
		return 1
	}
	defer before.Close()
	_, release, err := uninstall.OwnerLock(ctx, identity.DataHome, nil)
	if err != nil {
		cli.errorf("installation: %v", err)
		return 1
	}
	defer release()
	value := before.Installation
	if arguments[0] == "full" {
		if before.State == "matching" && value.BinDir != request.BinDir {
			cli.errorf("installation entrypoint ownership conflicts")
			return 1
		}
		value.BinDir = request.BinDir
		value.ShellFiles = slices.Clone(value.ShellFiles)
		for _, path := range request.ShellFiles {
			if !slices.Contains(value.ShellFiles, path) {
				value.ShellFiles = append(value.ShellFiles, path)
			}
		}
	} else if request.BinDir != "" || len(request.ShellFiles) != 0 {
		return 2
	}
	records := map[string]uninstall.Entry{}
	for _, record := range value.CacheFiles {
		records[record.Path] = record
	}
	if arguments[0] == "full" {
		runtime, readErr := uninstall.ReadInstallationFile(filepath.Join(identity.RuntimeRoot, uninstall.ManifestName), identity.DataHome, identity.OperatorUID)
		if readErr != nil {
			cli.errorf("installation: %v", readErr)
			return 1
		}
		if err := runtime.CheckRoots(identity.ConfigHome, identity.RuntimeRoot, identity.CacheRoot); err != nil {
			cli.errorf("installation: %v", config.OwnerMetadataContext(filepath.Join(identity.RuntimeRoot, uninstall.ManifestName), "runtime installation inventory", err))
			return 1
		}
		if err := runtime.CheckRuntimeScope(); err != nil {
			cli.errorf("installation: %v", config.OwnerMetadataContext(filepath.Join(identity.RuntimeRoot, uninstall.ManifestName), "runtime installation inventory", err))
			return 1
		}
		for _, record := range runtime.CacheFiles {
			records[record.Path] = record
		}
	}
	for _, path := range request.CacheInputs {
		relative, relErr := filepath.Rel(identity.CacheRoot, path)
		if relErr != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, "../") {
			continue
		}
		tree, readErr := uninstall.CaptureRoot(path)
		if readErr != nil || len(tree.Entries) != 1 || tree.Entries[0].Mode&syscall.S_IFMT != syscall.S_IFREG || tree.Entries[0].UID != identity.OperatorUID {
			cli.errorf("unsafe installation cache artifact")
			return 1
		}
		record := tree.Entries[0]
		record.Path = relative
		records[relative] = record
	}
	unchanged := len(records) == len(value.CacheFiles)
	for _, record := range value.CacheFiles {
		unchanged = unchanged && records[record.Path] == record
	}
	if !unchanged {
		value.CacheFiles = nil
		for _, record := range records {
			value.CacheFiles = append(value.CacheFiles, record)
		}
		sort.Slice(value.CacheFiles, func(i, j int) bool { return value.CacheFiles[i].Path < value.CacheFiles[j].Path })
	}
	if ctx.Err() != nil {
		return 1
	}
	if err := before.Publish(value); err != nil {
		cli.errorf("installation: %v", err)
		return 1
	}
	return 0
}

func (cli *CLI) installationIdentity(runtime, configuration string, includeBootstrap bool) (uninstall.Installation, error) {
	home := firstNonempty(cli.env["SUBYARD_OPERATOR_HOME"], cli.env["HOME"])
	data := firstNonempty(cli.env["SUBYARD_HOME"], filepath.Join(home, ".subyard"))
	if configuration == "" {
		configuration = firstNonempty(cli.env["SUBYARD_CONFIG_HOME"], filepath.Join(firstNonempty(cli.env["XDG_CONFIG_HOME"], filepath.Join(home, ".config")), "subyard"))
	}
	if runtime == "" {
		runtime = firstNonempty(cli.env["YARD_RUNTIME_ROOT"], filepath.Join(data, "runtime"))
	}
	return uninstall.InstallationIdentity(uint32(os.Getuid()), data, configuration, runtime, firstNonempty(cli.env["YARD_RELEASE_CACHE"], filepath.Join(data, "releases")), includeBootstrap)
}

type installationActivationReconciler struct {
	cli     *CLI
	request releasetransition.ProcessRequest
	before  *uninstall.InstallationInspection
}

func (*installationActivationReconciler) ID() string { return "installation-runtime-v1" }

func (r *installationActivationReconciler) Observe(_ context.Context, _ releasetransition.ReleasePair, _ releasetransition.ReleaseLinks) (releasetransition.V2ActivationObservation, error) {
	identity, err := r.cli.installationIdentity(r.request.RuntimeRoot, r.request.ConfigHome, true)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	if r.before != nil {
		r.before.Close()
	}
	r.before, err = uninstall.InspectInstallation(identity.RuntimeRoot, identity)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	var bootstrap config.PersistentBinding
	if _, err := os.Lstat(filepath.Join(identity.DataHome, uninstall.ManifestName)); !os.IsNotExist(err) {
		full, err := uninstall.InspectInstallation(identity.DataHome, identity)
		if err != nil {
			r.before.Close()
			return releasetransition.V2ActivationObservation{}, err
		}
		bootstrap = config.BindPersistentSnapshot(full.Snapshot)
		full.Close()
	}
	desired, err := activationStageFingerprint(identity)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	directory := r.before.Directory
	for index := range directory.Entries {
		directory.Entries[index].Size, directory.Entries[index].Modified, directory.Entries[index].Changed = 0, 0, 0
	}
	binding, err := activationStageFingerprint(struct {
		Directory uninstall.Tree
		Epoch     uint64
		File      config.PersistentBinding
		Bootstrap config.PersistentBinding
	}{directory, r.before.Epoch, config.BindPersistentSnapshot(r.before.Snapshot), bootstrap})
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	actual := desired
	if r.before.State == "missing" {
		actual, err = activationStageFingerprint(struct {
			Identity  uninstall.Installation
			Directory uninstall.Tree
			Epoch     uint64
		}{identity, r.before.Directory, r.before.Epoch})
	}
	return releasetransition.V2ActivationObservation{Binding: binding, Actual: actual, Desired: desired, Converged: r.before.State == "matching", Consequences: []string{"establish scoped runtime installation inventory without native yard ownership"}}, err
}

func (r *installationActivationReconciler) Reconcile(ctx context.Context, _ releasetransition.ReleaseLinks) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.before == nil {
		return errors.New("runtime installation enrollment was not assessed")
	}
	defer r.before.Close()
	borrowed, err := uninstall.BorrowOwnerLock(r.before.Installation.DataHome)
	if err != nil {
		return err
	}
	if borrowed != nil {
		defer borrowed.Close()
	} else {
		_, release, err := uninstall.OwnerLock(ctx, r.before.Installation.DataHome, nil)
		if err != nil {
			return err
		}
		defer release()
	}
	return r.before.Publish(r.before.Installation)
}
