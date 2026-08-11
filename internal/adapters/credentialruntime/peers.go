package credentialruntime

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/credential"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/shellquote"
)

func (runtime *Runtime) peerPath(name string) (string, error) {
	if !domain.SafeName(name) {
		return "", fmt.Errorf("invalid peer name %q", name)
	}
	return filepath.Join(runtime.peersDirectory, name+".json"), nil
}

func (runtime *Runtime) peers() ([]domain.CredentialPeer, error) {
	entries, err := os.ReadDir(runtime.peersDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return []domain.CredentialPeer{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]domain.CredentialPeer, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var peer domain.CredentialPeer
		if err := readProtectedJSON(filepath.Join(runtime.peersDirectory, entry.Name()), &peer); err != nil {
			return nil, err
		}
		if peer.Name != strings.TrimSuffix(entry.Name(), ".json") {
			return nil, fmt.Errorf("peer file %q does not match its name", entry.Name())
		}
		if err := credential.ValidatePeer(peer); err != nil {
			return nil, err
		}
		result = append(result, peer)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result, nil
}

func (runtime *Runtime) peer(name string) (domain.CredentialPeer, error) {
	path, err := runtime.peerPath(name)
	if err != nil {
		return domain.CredentialPeer{}, err
	}
	var peer domain.CredentialPeer
	if err := readProtectedJSON(path, &peer); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.CredentialPeer{}, fmt.Errorf("credential peer %q is not enrolled", name)
		}
		return domain.CredentialPeer{}, err
	}
	if peer.Name != name {
		return domain.CredentialPeer{}, errors.New("credential peer path does not match peer name")
	}
	if err := credential.ValidatePeer(peer); err != nil {
		return domain.CredentialPeer{}, err
	}
	return peer, nil
}

func (runtime *Runtime) peerByActor(actor string) (domain.CredentialPeer, bool, error) {
	peers, err := runtime.peers()
	if err != nil {
		return domain.CredentialPeer{}, false, err
	}
	for _, peer := range peers {
		if peer.ActorID == actor {
			return peer, true, nil
		}
	}
	return domain.CredentialPeer{}, false, nil
}

func (runtime *Runtime) recipientForActor(actor string) (string, error) {
	identity, err := runtime.Identity()
	if err != nil {
		return "", err
	}
	if actor == identity.ActorID {
		return identity.AgeRecipient, nil
	}
	peer, found, err := runtime.peerByActor(actor)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("no age recipient enrolled for actor %q", actor)
	}
	return peer.AgeRecipient, nil
}

func (runtime *Runtime) recipientActors() ([]string, error) {
	identity, err := runtime.Identity()
	if err != nil {
		return nil, err
	}
	result := []string{identity.ActorID}
	peers, err := runtime.peers()
	if err != nil {
		return nil, err
	}
	for _, peer := range peers {
		if peer.Trusted {
			result = append(result, peer.ActorID)
		}
	}
	return compactSorted(result), nil
}

func (runtime *Runtime) storePeer(
	name string,
	identity Identity,
	transport, destination, remoteYard string,
	manualOnly bool,
) (domain.CredentialPeer, error) {
	if err := validateIdentity(identity); err != nil {
		return domain.CredentialPeer{}, err
	}
	requestedPath, err := runtime.peerPath(name)
	if err != nil {
		return domain.CredentialPeer{}, err
	}
	var existing *domain.CredentialPeer
	var existingPath string
	if _, err := os.Stat(requestedPath); err == nil {
		peer, err := runtime.peer(name)
		if err != nil {
			return domain.CredentialPeer{}, err
		}
		existing, existingPath = &peer, requestedPath
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.CredentialPeer{}, err
	} else if peer, found, err := runtime.peerByActor(identity.ActorID); err != nil {
		return domain.CredentialPeer{}, err
	} else if found {
		existing = &peer
		existingPath, _ = runtime.peerPath(peer.Name)
	}
	incoming := domain.CredentialPeer{
		SchemaVersion: credentialSchema, Name: name, ActorID: identity.ActorID,
		AgeRecipient: identity.AgeRecipient, SigningPublic: identity.SigningPublic,
		Transport: transport, Dest: destination, RemoteYard: remoteYard,
		ManualOnly: manualOnly, Trusted: true,
	}
	merged, err := credential.MergePeer(incoming, existing)
	if err != nil {
		return domain.CredentialPeer{}, err
	}
	payload, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return domain.CredentialPeer{}, err
	}
	path, _ := runtime.peerPath(merged.Name)
	if err := atomicWrite(path, append(payload, '\n'), 0o600); err != nil {
		return domain.CredentialPeer{}, err
	}
	if existingPath != "" && existingPath != path {
		if err := os.Remove(existingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return domain.CredentialPeer{}, err
		}
	}
	if err := runtime.addAllowedSigner(merged.ActorID, merged.SigningPublic); err != nil {
		return domain.CredentialPeer{}, err
	}
	return merged, nil
}

func (runtime *Runtime) rebuildAllowedSigners() error {
	identity, err := runtime.Identity()
	if err != nil {
		return err
	}
	if err := atomicWrite(runtime.allowedSigners, nil, 0o600); err != nil {
		return err
	}
	if err := runtime.addAllowedSigner(identity.ActorID, identity.SigningPublic); err != nil {
		return err
	}
	peers, err := runtime.peers()
	if err != nil {
		return err
	}
	for _, peer := range peers {
		if err := runtime.addAllowedSigner(peer.ActorID, peer.SigningPublic); err != nil {
			return err
		}
	}
	return nil
}

func peerRole(peer domain.CredentialPeer) (string, error) {
	switch peer.Transport {
	case "local", "ssh":
		return "active", nil
	case "inbound":
		return "passive", nil
	default:
		return "", fmt.Errorf("peer %q has invalid transport %q", peer.Name, peer.Transport)
	}
}

func peerAssignment(peer domain.CredentialPeer) (string, error) {
	contextName := peer.Name
	if peer.Transport == "ssh" {
		contextName = firstNonEmpty(peer.RemoteYard, "default")
	} else if peer.Transport != "local" {
		return "", errors.New("passive peer has no assignment route")
	}
	if !domain.SafeName(contextName) || !domain.SafeID(peer.ActorID) {
		return "", errors.New("peer assignment is invalid")
	}
	return peer.ActorID + "/" + contextName, nil
}

func (runtime *Runtime) callTarget(
	ctx context.Context,
	target Target,
	arguments []string,
	stdin io.Reader,
) ([]byte, error) {
	if !domain.SafeName(target.Name) {
		return nil, errors.New("invalid credential target")
	}
	switch target.Transport {
	case "local":
		args := make([]string, 0, len(arguments)+2)
		if target.Name != "default" {
			args = append(args, "-Y", target.Name)
		}
		args = append(args, arguments...)
		return runtime.runWithEnvironment(
			ctx, runtime.config.Dispatcher, args, stdin,
			runtime.config.TargetEnvironment, nil,
		)
	case "ssh":
		if !domain.SafeSSHTarget(target.Destination) ||
			target.RemoteYard != "" && !domain.SafeName(target.RemoteYard) {
			return nil, errors.New("invalid SSH credential target")
		}
		remote := []string{"yard"}
		if target.RemoteYard != "" {
			remote = append(remote, "-Y", target.RemoteYard)
		}
		remote = append(remote, arguments...)
		command := shellquote.Command(remote)
		args := []string{
			"-o", "BatchMode=yes", "-o", "ConnectTimeout=" + strconv.Itoa(runtime.sshTimeout),
			target.Destination, "--", "bash", "-lc", shellquote.Word(command),
		}
		return runtime.runWithEnvironment(
			ctx, "ssh", args, stdin, runtime.config.TargetEnvironment, nil,
		)
	default:
		return nil, fmt.Errorf("unknown credential transport %q", target.Transport)
	}
}

func (runtime *Runtime) callPeer(
	ctx context.Context,
	peer domain.CredentialPeer,
	arguments []string,
	stdin io.Reader,
) ([]byte, error) {
	role, err := peerRole(peer)
	if err != nil {
		return nil, err
	}
	if role != "active" {
		return nil, fmt.Errorf("peer %q has no reverse transport; sync it from its controller", peer.Name)
	}
	target := Target{
		Name: peer.Name, Transport: peer.Transport, Destination: peer.Dest, RemoteYard: peer.RemoteYard,
	}
	return runtime.callTarget(ctx, target, arguments, stdin)
}

func (runtime *Runtime) callPeerContext(
	ctx context.Context,
	peer domain.CredentialPeer,
	contextName string,
	arguments []string,
) ([]byte, error) {
	if !domain.SafeName(contextName) {
		return nil, errors.New("invalid yard context")
	}
	target := Target{Name: contextName, Transport: peer.Transport, Destination: peer.Dest}
	if peer.Transport == "ssh" && contextName != "default" {
		target.RemoteYard = contextName
	}
	return runtime.callTarget(ctx, target, arguments, nil)
}

func (runtime *Runtime) resolveTarget(ctx context.Context, name string) (Target, error) {
	if runtime.config.Resolve == nil {
		return Target{}, errors.New("credential target resolver is unavailable")
	}
	return runtime.config.Resolve(ctx, name)
}

func (runtime *Runtime) refreshShared(ctx context.Context) error {
	if _, err := runtime.gitRun(ctx, runtime.shared, "fetch", "-q", "origin", "main"); err != nil {
		return err
	}
	if _, err := runtime.gitRun(ctx, runtime.shared, "merge", "--ff-only", "-q", "origin/main"); err != nil {
		return errors.New("shared key checkout diverged from its local bare repository")
	}
	return nil
}

func (runtime *Runtime) statePath(peer string) (string, error) {
	if !domain.SafeName(peer) {
		return "", errors.New("invalid sync peer")
	}
	return filepath.Join(runtime.stateDirectory, peer+".json"), nil
}

func (runtime *Runtime) readState(peer string) (domain.CredentialSyncState, error) {
	path, err := runtime.statePath(peer)
	if err != nil {
		return domain.CredentialSyncState{}, err
	}
	var state domain.CredentialSyncState
	if err := readProtectedJSON(path, &state); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, err
	}
	if state.Peer != "" && state.Peer != peer {
		return state, errors.New("sync state belongs to another peer")
	}
	return state, credential.ValidateSyncState(state)
}

func (runtime *Runtime) writeState(peer string, success bool, message, head string) error {
	current, err := runtime.readState(peer)
	if err != nil {
		return err
	}
	next, err := credential.NextSyncState(current, peer, time.Now().Unix(), success, message, head,
		time.Duration(positiveInt(runtime.env["SUBYARD_KEYS_SUCCESS_RETRY_SECONDS"], 21600))*time.Second)
	if err != nil {
		return err
	}
	payload, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	path, _ := runtime.statePath(peer)
	return atomicWrite(path, append(payload, '\n'), 0o600)
}

func (runtime *Runtime) peerGitURL(ctx context.Context, peer domain.CredentialPeer) (string, error) {
	pathBytes, err := runtime.callPeer(ctx, peer, []string{"_keys-exchange", "bare-path"}, nil)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(pathBytes))
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00 :'\"`$;&|(){}[]<>\\") {
		return "", errors.New("peer did not expose a safe ledger path")
	}
	if peer.Transport == "ssh" {
		return peer.Dest + ":" + path, nil
	}
	return path, nil
}

func (runtime *Runtime) gitWithPeerEnvironment(
	ctx context.Context,
	peer domain.CredentialPeer,
	arguments ...string,
) ([]byte, error) {
	args := []string{"-C", runtime.shared, "-c", "core.hooksPath=/dev/null"}
	args = append(args, arguments...)
	extra := map[string]string{}
	if peer.Transport == "ssh" {
		extra["GIT_SSH_COMMAND"] = fmt.Sprintf("ssh -o BatchMode=yes -o ConnectTimeout=%d", runtime.sshTimeout)
	}
	return runtime.run(ctx, runtime.git, args, nil, extra)
}

func (runtime *Runtime) syncPeer(ctx context.Context, peerName string) error {
	peer, err := runtime.peer(peerName)
	if err != nil {
		return err
	}
	return runtime.syncPreparedPeer(ctx, peer)
}

func (runtime *Runtime) syncPreparedPeer(ctx context.Context, peer domain.CredentialPeer) error {
	if role, err := peerRole(peer); err != nil || role != "active" {
		if err != nil {
			return err
		}
		return fmt.Errorf("peer %q is passive (respond-only)", peer.Name)
	}
	head, err := runtime.syncPeerOnce(ctx, peer)
	if err == nil {
		if stateErr := runtime.writeState(peer.Name, true, "", head); stateErr != nil {
			return stateErr
		}
		fmt.Fprintf(runtime.config.Stderr, "  [ ok ] credential ledger synchronized with %q\n", peer.Name)
		return nil
	}
	message := strings.ReplaceAll(strings.TrimSpace(err.Error()), "\n", " ")
	if len(message) > 300 {
		message = message[:300]
	}
	if stateErr := runtime.writeState(peer.Name, false, message, ""); stateErr != nil {
		return errors.Join(err, stateErr)
	}
	return fmt.Errorf("credential sync with %q failed: %s", peer.Name, message)
}

func (runtime *Runtime) syncPeerOnce(ctx context.Context, peer domain.CredentialPeer) (string, error) {
	url, err := runtime.peerGitURL(ctx, peer)
	if err != nil {
		return "", err
	}
	ref := "refs/remotes/keys/" + peer.Name
	if err := runtime.refreshShared(ctx); err != nil {
		return "", err
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := runtime.gitWithPeerEnvironment(ctx, peer, "fetch", "-q", "--no-tags", url,
			"+refs/heads/main:"+ref); err != nil {
			return "", err
		}
		if err := runtime.validateFetched(ctx, peer, ref); err != nil {
			return "", err
		}
		if err := runtime.mergeFetched(ctx, ref); err != nil {
			return "", err
		}
		conflicts, err := runtime.reconcileShared(ctx)
		if err != nil {
			return "", err
		}
		if _, err := runtime.gitRun(ctx, runtime.shared, "push", "-q", "origin", "main"); err != nil {
			return "", err
		}
		if len(conflicts) != 0 {
			fmt.Fprintf(runtime.config.Stderr, "  [warn] unresolved credential heads after sync: %s\n", strings.Join(conflicts, " "))
		}
		if _, err := runtime.gitWithPeerEnvironment(ctx, peer, "push", "-q", url, "main:main"); err != nil {
			if attempt < 3 {
				fmt.Fprintf(runtime.config.Stderr, "  [warn] peer %q advanced during sync; retrying (%d/3)\n", peer.Name, attempt)
				continue
			}
			return "", fmt.Errorf("peer %q kept advancing; sync did not converge after 3 attempts", peer.Name)
		}
		identity, err := runtime.Identity()
		if err != nil {
			return "", err
		}
		if _, err := runtime.callPeer(ctx, peer, []string{"_keys-exchange", "refresh", identity.ActorID}, nil); err != nil {
			return "", err
		}
		_ = runtime.materializeAll(ctx, "", true)
		headBytes, err := runtime.gitRun(ctx, runtime.shared, "rev-parse", "main")
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(headBytes)), nil
	}
	return "", errors.New("credential sync retry loop ended unexpectedly")
}

func (runtime *Runtime) validateFetched(ctx context.Context, peer domain.CredentialPeer, ref string) error {
	state, err := runtime.readState(peer.Name)
	if err != nil {
		return err
	}
	if state.LastHead != "" {
		if _, err := runtime.gitRun(ctx, runtime.shared, "cat-file", "-e", state.LastHead+"^{commit}"); err != nil {
			return fmt.Errorf("recorded peer head %s is missing from the local ledger", state.LastHead)
		}
		if _, err := runtime.gitRun(ctx, runtime.shared, "merge-base", "--is-ancestor", state.LastHead, ref); err != nil {
			return fmt.Errorf("peer %q rewrote or removed previously observed Git history", peer.Name)
		}
		deleted, err := runtime.gitRun(ctx, runtime.shared, "diff", "--diff-filter=D", "--name-only", state.LastHead+".."+ref, "--", "records")
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(deleted)) != "" {
			return fmt.Errorf("peer %q deleted immutable revision objects", peer.Name)
		}
	}
	commits, err := runtime.gitRun(ctx, runtime.shared, "rev-list", ref, "--not", "main")
	if err != nil {
		return err
	}
	for _, commit := range strings.Fields(string(commits)) {
		args := []string{
			"-C", runtime.shared, "-c", "core.hooksPath=/dev/null",
			"-c", "gpg.ssh.allowedSignersFile=" + runtime.allowedSigners, "verify-commit", commit,
		}
		if _, err := runtime.run(ctx, runtime.git, args, nil, nil); err != nil {
			return fmt.Errorf("peer %q has a commit without an allowed SSH signature", peer.Name)
		}
	}
	reverse, err := runtime.gitRun(ctx, runtime.shared, "rev-list", "--reverse", ref, "--not", "main")
	if err != nil {
		return err
	}
	for _, commit := range strings.Fields(string(reverse)) {
		changes, err := runtime.gitRun(ctx, runtime.shared, "diff-tree", "-m", "--root", "--no-commit-id", "--name-status", "-r", commit, "--", "records")
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(changes)), "\n") {
			if line != "" && !strings.HasPrefix(line, "A\t") {
				return fmt.Errorf("peer %q modified or deleted an immutable revision object", peer.Name)
			}
		}
	}
	temporary, err := os.MkdirTemp("", ".subyard-ledger-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	archive, err := runtime.gitRun(ctx, runtime.shared, "archive", ref)
	if err != nil {
		return err
	}
	if err := extractLedgerArchive(archive, temporary); err != nil {
		return err
	}
	if err := runtime.verifyTree(ctx, temporary); err != nil {
		runtime.quarantineTree(temporary)
		return fmt.Errorf("peer %q sent an invalid revision: %w", peer.Name, err)
	}
	return nil
}

func extractLedgerArchive(payload []byte, destination string) error {
	reader := tar.NewReader(bytes.NewReader(payload))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return errors.New("ledger archive contains an unsafe path")
		}
		path := filepath.Join(destination, name)
		if !pathWithin(path, destination) {
			return errors.New("ledger archive escapes extraction root")
		}
		switch header.Typeflag {
		case tar.TypeXGlobalHeader:
			continue
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if header.Size < 0 || header.Size > maximumOutput {
				return errors.New("ledger archive file exceeds size limit")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			data, err := io.ReadAll(io.LimitReader(reader, maximumOutput+1))
			if err != nil || int64(len(data)) != header.Size {
				return errors.New("ledger archive file is truncated")
			}
			if err := atomicWrite(path, data, 0o600); err != nil {
				return err
			}
		default:
			return errors.New("ledger archive contains a non-regular entry")
		}
	}
}

func (runtime *Runtime) verifyTree(ctx context.Context, root string) error {
	var records []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("ledger tree contains a symlink")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		allowed := relative == ".ledger" || len(parts) == 3 && parts[0] == "records" &&
			(strings.HasSuffix(parts[2], ".json") || strings.HasSuffix(parts[2], ".json.sig"))
		if !allowed {
			return fmt.Errorf("unexpected ledger path %q", relative)
		}
		if strings.HasSuffix(path, ".json") {
			records = append(records, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	incoming := make([]domain.CredentialMetadata, 0, len(records))
	identity, err := runtime.Identity()
	if err != nil {
		return err
	}
	for _, path := range records {
		if err := runtime.verifyRecord(ctx, root, path); err != nil {
			return err
		}
		metadata, err := runtime.readRecordMetadata(path)
		if err != nil {
			return err
		}
		incoming = append(incoming, metadata)
		for _, actor := range metadata.RecipientActors {
			if actor == identity.ActorID {
				payload, err := runtime.decryptPath(ctx, root, path)
				clear(payload)
				if err != nil {
					return err
				}
				break
			}
		}
	}
	existing, err := runtime.records(ctx, sharedLedger)
	if err != nil {
		return err
	}
	return credential.ValidateIncomingRevisions(incoming, existing)
}

func (runtime *Runtime) quarantineTree(root string) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil
		}
		destination := filepath.Join(runtime.quarantine, relative)
		if payload, err := os.ReadFile(path); err == nil {
			_ = atomicWrite(destination, payload, 0o600)
		}
		if payload, err := os.ReadFile(path + ".sig"); err == nil {
			_ = atomicWrite(destination+".sig", payload, 0o600)
		}
		return nil
	})
}

func (runtime *Runtime) mergeFetched(ctx context.Context, ref string) error {
	_, relatedErr := runtime.gitRun(ctx, runtime.shared, "merge-base", "main", ref)
	arguments := []string{"merge", "-q", "-S", "--no-edit"}
	if relatedErr != nil {
		arguments = append(arguments, "--allow-unrelated-histories")
	}
	arguments = append(arguments, ref)
	if _, err := runtime.gitSigned(ctx, runtime.shared, arguments...); err != nil {
		_, _ = runtime.gitRun(ctx, runtime.shared, "merge", "--abort")
		return errors.New("append-only ledger merge conflicted")
	}
	return nil
}

type scopedCrypto struct {
	runtime *Runtime
	scope   ledgerScope
}

func (adapter scopedCrypto) Decrypt(ctx context.Context, metadata domain.CredentialMetadata, output io.Writer) error {
	payload, err := adapter.runtime.decrypt(ctx, adapter.scope, metadata)
	if err != nil {
		return err
	}
	defer clear(payload)
	_, err = output.Write(payload)
	return err
}

func (adapter scopedCrypto) Verify(ctx context.Context, metadata domain.CredentialMetadata) error {
	return adapter.runtime.verifyRecord(ctx, adapter.runtime.repository(adapter.scope),
		adapter.runtime.recordPath(adapter.scope, metadata.CredentialID, metadata.RevisionID))
}

var _ ports.CredentialCrypto = scopedCrypto{}

func (runtime *Runtime) reconcileShared(ctx context.Context) ([]string, error) {
	revisions, err := runtime.records(ctx, sharedLedger)
	if err != nil {
		return nil, err
	}
	byCredential := make(map[string][]domain.CredentialMetadata)
	for _, revision := range revisions {
		byCredential[revision.CredentialID] = append(byCredential[revision.CredentialID], revision)
	}
	credentials := make([]string, 0, len(byCredential))
	for credentialID := range byCredential {
		credentials = append(credentials, credentialID)
	}
	sort.Strings(credentials)
	var conflicts []string
	for _, credentialID := range credentials {
		heads := credential.Heads(byCredential[credentialID], credentialID)
		decision, err := credential.AnalyzeHeads(ctx, heads, scopedCrypto{runtime: runtime, scope: sharedLedger})
		if err != nil {
			return nil, err
		}
		if !decision.RequiresMerge {
			continue
		}
		if decision.Conflict {
			conflicts = append(conflicts, credentialID)
			continue
		}
		var payload []byte
		if decision.State == "active" {
			payload, err = runtime.decrypt(ctx, sharedLedger, heads[0])
			if err != nil {
				return nil, err
			}
		}
		spec := specFromMetadata(decision.Template)
		spec.State = decision.State
		spec.Parents = decision.Parents
		spec.RecipientActors = decision.Recipients
		spec.Exclusive = decision.Exclusive
		_, err = runtime.publish(ctx, sharedLedger, spec, payload)
		clear(payload)
		if err != nil {
			return nil, err
		}
	}
	return conflicts, nil
}

func specFromMetadata(metadata domain.CredentialMetadata) revisionSpec {
	return revisionSpec{
		CredentialID: metadata.CredentialID, Label: metadata.Label, Kind: metadata.Kind,
		Zone: metadata.Zone, Consumer: metadata.Consumer, State: metadata.State,
		RecipientActors: append([]string(nil), metadata.RecipientActors...), Exclusive: metadata.Exclusive,
		Syncable: metadata.Syncable, AuthorityHost: metadata.AuthorityHost,
		AssignedYard: metadata.AssignedYard, AssignmentEpoch: metadata.AssignmentEpoch,
	}
}

func (runtime *Runtime) syncAll(ctx context.Context, ifDue bool) error {
	peers, err := runtime.peers()
	if err != nil {
		return err
	}
	var failures []error
	for _, peer := range peers {
		role, err := peerRole(peer)
		if err != nil {
			return err
		}
		if role != "active" || peer.ManualOnly {
			continue
		}
		if ifDue {
			state, err := runtime.readState(peer.Name)
			if err != nil {
				return err
			}
			due, err := credential.SyncDue(state, time.Now().Unix(),
				time.Duration(positiveInt(runtime.env["SUBYARD_KEYS_IF_DUE_SECONDS"], 3600))*time.Second)
			if err != nil {
				return err
			}
			if !due {
				continue
			}
		}
		if err := runtime.syncPeer(ctx, peer.Name); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
