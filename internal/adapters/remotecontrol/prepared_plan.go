package remotecontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
)

func (runtime Runtime) preparedBinding(prepared domain.RemotePrepared) (string, error) {
	type file struct {
		Path          string
		Exists        bool
		Mode          os.FileMode
		Size          int64
		Modified      int64
		Device, Inode uint64
		UID, GID      uint32
		Changed       syscall.Timespec
		Content       []byte
	}
	paths := []string{runtime.snippetPath(prepared.Spec.LegacyAlias), runtime.sshConfigPath(), runtime.knownHostsPath(), runtime.cachePath(prepared.Spec.LegacyAlias)}
	contextPath := filepath.Join(runtime.ConfigHome, "yards", prepared.Spec.LegacyAlias, "config.env")
	if prepared.Existing != nil {
		contextPath = prepared.Existing.Path
	}
	paths = append(paths, contextPath)
	identityPaths := make(map[string]bool)
	if runtime.PublicKey != "" && !strings.HasSuffix(runtime.PublicKey, ".pub") {
		return "", errors.New("controller public key path must end in .pub")
	}
	for _, public := range []string{runtime.PublicKey, filepath.Join(runtime.Home, ".ssh", "id_ed25519.pub"), filepath.Join(runtime.Home, ".ssh", "id_ecdsa.pub"), filepath.Join(runtime.Home, ".ssh", "id_rsa.pub"), filepath.Join(runtime.DataHome, "ssh", "id_ed25519.pub")} {
		if public == "" {
			continue
		}
		identityPaths[public] = true
		identityPaths[public[:len(public)-len(filepath.Ext(public))]] = true
	}
	for path := range identityPaths {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	files := make([]file, 0, len(paths))
	for _, path := range paths {
		fact := file{Path: path}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			files = append(files, fact)
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || (!identityPaths[path] && info.Mode().Perm()&0o077 != 0) || (identityPaths[path] && !strings.HasSuffix(path, ".pub") && info.Mode().Perm()&0o077 != 0) {
			return "", errors.New("unsafe remote configuration target")
		}
		fact.Exists, fact.Mode = true, info.Mode()
		fact.Size, fact.Modified = info.Size(), info.ModTime().UnixNano()
		if identity, ok := info.Sys().(*syscall.Stat_t); ok {
			fact.Device, fact.Inode, fact.UID, fact.GID, fact.Changed = uint64(identity.Dev), identity.Ino, identity.Uid, identity.Gid, identity.Ctim
		}
		if !identityPaths[path] || strings.HasSuffix(path, ".pub") {
			fact.Content, err = os.ReadFile(path)
			if err != nil {
				return "", err
			}
		}
		files = append(files, fact)
	}
	payload, err := json.Marshal(files)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(payload)), nil
}

func verifyRemoteFiles(expected map[string][]byte) error {
	for path, payload := range expected {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return errors.New("remote configuration verification failed")
		}
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, payload) {
			return errors.New("remote configuration readback differs from the approved state")
		}
	}
	return nil
}

func (runtime Runtime) CapturePrepared(_ context.Context, prepared domain.RemotePrepared) (domain.RemotePrepared, error) {
	var err error
	prepared.Binding, err = runtime.preparedBinding(prepared)
	if err != nil {
		return prepared, err
	}
	target := "controller context " + prepared.Spec.LegacyAlias
	step := func(id, scope, desired, verify, consequence string, decision domain.StepDecision) domain.OperationStep {
		return domain.OperationStep{ID: "remote." + id, Target: scope, Observed: "captured local registration and trust metadata", Desired: desired, Decision: decision,
			Preconditions: []string{"captured local context, SSH configuration and trust are unchanged", "the approved owner and yard identity remain valid"}, Verify: verify, Consequence: consequence}
	}
	switch prepared.Action {
	case domain.RemoteAdd:
		keys := remoteKeyFingerprints(prepared.Scanned)
		prepared.Steps = []domain.OperationStep{
			step("authorize", prepared.Spec.OwnerEndpoint+":"+prepared.Spec.OwnerYardName, "authorize one controller identity for the approved yard", "owner authorization completes and the approved data plane accepts the controller identity", "authorize the controller key on the approved owner yard", domain.StepConditional),
			step("register", target, fmt.Sprintf("SSH port %d, user %s, yard SSH keys [%s]", prepared.Owner.SSHPort, prepared.Owner.DevUser, keys), "read exact installed local files and verify the scanned SSH key through the data plane", "install the local remote context, SSH alias and trust pin; yard ssh key: "+keys, domain.StepApply),
		}
	case domain.RemoteRepairKey:
		prepared.Steps = []domain.OperationStep{step("trust", target, "yard SSH keys ["+remoteKeyFingerprints(prepared.Scanned)+"]", "data-plane key matches the approved owner scan", "replace this context's SSH trust pin; recorded yard ssh key: "+remoteKeyFingerprints(prepared.Recorded)+"; new yard ssh key: "+remoteKeyFingerprints(prepared.Scanned), domain.StepApply)}
	case domain.RemoteRemove:
		prepared.Steps = []domain.OperationStep{step("remove", target, "local context, alias, cache and yard trust pin absent; remote yard and projects retained", "read local targets and verify absence of this context and trust pin", "remove this controller context, SSH alias, cache and yard trust pin", domain.StepApply)}
	}
	return prepared, domain.ValidateOperationSteps(prepared.Steps)
}

func remoteKeyFingerprints(keys []domain.RemoteKey) string {
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key.Fingerprint)
	}
	return strings.Join(values, ", ")
}

func (runtime Runtime) checkPreparedBinding(prepared domain.RemotePrepared) error {
	if prepared.Binding == "" {
		return nil
	}
	fresh, err := runtime.preparedBinding(prepared)
	if err != nil {
		return err
	}
	if fresh != prepared.Binding {
		return fmt.Errorf("%w: remote registration or trust changed after planning", domain.ErrPlanStale)
	}
	return nil
}
