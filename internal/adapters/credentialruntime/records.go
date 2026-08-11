package credentialruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/credential"
	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/term"
)

type ledgerScope string

const (
	sharedLedger ledgerScope = "shared"
	localLedger  ledgerScope = "local"
)

type revisionSpec struct {
	CredentialID    string
	Label           string
	Kind            string
	Zone            string
	Consumer        string
	State           string
	RecipientActors []string
	Exclusive       bool
	Syncable        bool
	AuthorityHost   string
	AssignedYard    string
	AssignmentEpoch int64
	Parents         []string
}

type recordEnvelope struct {
	domain.CredentialMetadata
	Payload string          `json:"payload"`
	SOPS    json.RawMessage `json:"sops,omitempty"`
}

var recordKeys = map[string]struct{}{
	"schemaVersion": {}, "credentialId": {}, "revisionId": {}, "label": {}, "kind": {},
	"zone": {}, "scope": {}, "consumer": {}, "syncable": {}, "exclusive": {},
	"recipientActors": {}, "authorityHost": {}, "assignedYard": {}, "assignmentEpoch": {},
	"actorId": {}, "actorCounter": {}, "parents": {}, "timestamp": {}, "state": {},
	"payload": {}, "sops": {},
}

func (runtime *Runtime) repository(scope ledgerScope) string {
	if scope == localLedger {
		return runtime.local
	}
	return runtime.shared
}

func (runtime *Runtime) recordPath(scope ledgerScope, credentialID, revisionID string) string {
	return filepath.Join(runtime.repository(scope), "records", credentialID, revisionID+".json")
}

func (runtime *Runtime) records(ctx context.Context, scope ledgerScope) ([]domain.CredentialMetadata, error) {
	recordsRoot := filepath.Join(runtime.repository(scope), "records")
	result := make([]domain.CredentialMetadata, 0)
	err := filepath.WalkDir(recordsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) && path == recordsRoot {
				return fs.SkipDir
			}
			return walkErr
		}
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("credential record path is a symlink: %s", path)
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		metadata, err := runtime.readRecordMetadata(path)
		if err != nil {
			return err
		}
		result = append(result, metadata)
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := credential.ValidateRevisions(result); err != nil {
		return nil, err
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].CredentialID != result[right].CredentialID {
			return result[left].CredentialID < result[right].CredentialID
		}
		return result[left].RevisionID < result[right].RevisionID
	})
	return result, nil
}

func (runtime *Runtime) allRecords(ctx context.Context) (map[ledgerScope][]domain.CredentialMetadata, error) {
	result := make(map[ledgerScope][]domain.CredentialMetadata, 2)
	for _, scope := range []ledgerScope{sharedLedger, localLedger} {
		records, err := runtime.records(ctx, scope)
		if err != nil {
			return nil, fmt.Errorf("read %s credential ledger: %w", scope, err)
		}
		result[scope] = records
	}
	return result, nil
}

func (runtime *Runtime) readRecordMetadata(path string) (domain.CredentialMetadata, error) {
	envelope, err := readRecord(path)
	if err != nil {
		return domain.CredentialMetadata{}, err
	}
	metadata := envelope.CredentialMetadata
	if err := credential.ValidateMetadata(metadata); err != nil {
		return domain.CredentialMetadata{}, fmt.Errorf("invalid credential record %s: %w", path, err)
	}
	if filepath.Base(filepath.Dir(path)) != metadata.CredentialID ||
		strings.TrimSuffix(filepath.Base(path), ".json") != metadata.RevisionID {
		return domain.CredentialMetadata{}, fmt.Errorf("credential record path does not match identity: %s", path)
	}
	return metadata, nil
}

func readRecord(path string) (recordEnvelope, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return recordEnvelope{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maximumOutput {
		return recordEnvelope{}, errors.New("credential record is not a bounded regular file")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return recordEnvelope{}, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(payload, &keys); err != nil {
		return recordEnvelope{}, err
	}
	for key := range keys {
		if _, allowed := recordKeys[key]; !allowed {
			return recordEnvelope{}, fmt.Errorf("credential record contains unknown field %q", key)
		}
	}
	var envelope recordEnvelope
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&envelope); err != nil {
		return recordEnvelope{}, err
	}
	if len(envelope.SOPS) == 0 || bytes.Equal(envelope.SOPS, []byte("null")) ||
		envelope.State == "active" && envelope.Payload == "" {
		return recordEnvelope{}, errors.New("credential record is not encrypted")
	}
	return envelope, nil
}

func (runtime *Runtime) findScope(credentialID string) (ledgerScope, error) {
	if !validCredentialID(credentialID) {
		return "", fmt.Errorf("invalid credential ID %q", credentialID)
	}
	shared := directoryExists(filepath.Join(runtime.shared, "records", credentialID))
	local := directoryExists(filepath.Join(runtime.local, "records", credentialID))
	if shared && local {
		return "", fmt.Errorf("credential ID %q exists in both ledgers", credentialID)
	}
	if shared {
		return sharedLedger, nil
	}
	if local {
		return localLedger, nil
	}
	return "", fmt.Errorf("unknown credential %q", credentialID)
}

func (runtime *Runtime) publish(ctx context.Context, scope ledgerScope, spec revisionSpec, payload []byte) (domain.CredentialMetadata, error) {
	identity, err := runtime.Identity()
	if err != nil {
		return domain.CredentialMetadata{}, err
	}
	if spec.CredentialID == "" {
		random, err := randomHex(16)
		if err != nil {
			return domain.CredentialMetadata{}, err
		}
		spec.CredentialID = "cred-" + random
	}
	counter, err := runtime.nextCounter()
	if err != nil {
		return domain.CredentialMetadata{}, err
	}
	suffix, err := randomHex(4)
	if err != nil {
		return domain.CredentialMetadata{}, err
	}
	metadata := domain.CredentialMetadata{
		SchemaVersion: credentialSchema, CredentialID: spec.CredentialID,
		RevisionID: fmt.Sprintf("%s-%012d-%s", identity.ActorID, counter, suffix),
		Parents:    append([]string(nil), spec.Parents...), Label: spec.Label, Kind: spec.Kind,
		Zone: spec.Zone, Scope: "staging", Consumer: spec.Consumer, State: spec.State,
		RecipientActors: append([]string(nil), spec.RecipientActors...), Exclusive: spec.Exclusive,
		Syncable: spec.Syncable, AuthorityHost: spec.AuthorityHost, AssignedYard: spec.AssignedYard,
		AssignmentEpoch: spec.AssignmentEpoch, ActorID: identity.ActorID, ActorCounter: counter,
		Timestamp: runtime.now(),
	}
	if err := credential.ValidateMetadata(metadata); err != nil {
		return domain.CredentialMetadata{}, err
	}
	existing, err := runtime.records(ctx, scope)
	if err != nil {
		return domain.CredentialMetadata{}, err
	}
	if err := credential.ValidateRevisions(append(existing, metadata)); err != nil {
		return domain.CredentialMetadata{}, err
	}
	recipients, err := runtime.ageRecipients(spec.RecipientActors)
	if err != nil {
		return domain.CredentialMetadata{}, err
	}
	directory := filepath.Dir(runtime.recordPath(scope, metadata.CredentialID, metadata.RevisionID))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return domain.CredentialMetadata{}, err
	}
	plain, err := os.CreateTemp("", ".subyard-credential-*")
	if err != nil {
		return domain.CredentialMetadata{}, err
	}
	plainPath := plain.Name()
	defer os.Remove(plainPath)
	if err := plain.Chmod(0o600); err != nil {
		plain.Close()
		return domain.CredentialMetadata{}, err
	}
	envelope := recordEnvelope{CredentialMetadata: metadata, Payload: base64.StdEncoding.EncodeToString(payload)}
	encoder := json.NewEncoder(plain)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(envelope); err != nil {
		plain.Close()
		return domain.CredentialMetadata{}, err
	}
	if err := plain.Close(); err != nil {
		return domain.CredentialMetadata{}, err
	}
	destination := runtime.recordPath(scope, metadata.CredentialID, metadata.RevisionID)
	temporary := destination + ".tmp"
	os.Remove(temporary)
	output, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return domain.CredentialMetadata{}, err
	}
	command := execCommandContext(ctx, runtime.sops,
		"encrypt", "--age", strings.Join(recipients, ","), "--encrypted-regex", "^(payload)$",
		"--input-type", "json", "--output-type", "json", plainPath)
	command.Dir = runtime.config.RepositoryRoot
	command.Env = runtime.config.Environment
	command.Stdout = output
	stderr := &limitedBuffer{limit: maximumOutput}
	command.Stderr = stderr
	runErr := command.Run()
	closeErr := output.Close()
	if runErr != nil || closeErr != nil {
		os.Remove(temporary)
		if runErr != nil {
			return domain.CredentialMetadata{}, errors.New(firstNonEmpty(strings.TrimSpace(stderr.String()), "SOPS could not encrypt credential revision"))
		}
		return domain.CredentialMetadata{}, closeErr
	}
	if err := os.Rename(temporary, destination); err != nil {
		os.Remove(temporary)
		return domain.CredentialMetadata{}, err
	}
	if err := runtime.signRecord(ctx, destination); err != nil {
		return domain.CredentialMetadata{}, err
	}
	if err := runtime.commitRecord(ctx, scope, metadata); err != nil {
		return domain.CredentialMetadata{}, err
	}
	return metadata, nil
}

// execCommandContext is a variable so record construction can stay explicit in tests without a shell.
var execCommandContext = func(ctx context.Context, program string, arguments ...string) *exec.Cmd {
	return exec.CommandContext(ctx, program, arguments...)
}

func (runtime *Runtime) commitRecord(ctx context.Context, scope ledgerScope, metadata domain.CredentialMetadata) error {
	repository := runtime.repository(scope)
	if _, err := runtime.gitRun(ctx, repository, "add", "--all"); err != nil {
		return err
	}
	message := fmt.Sprintf("Record %s revision %s", metadata.CredentialID, metadata.RevisionID)
	if _, err := runtime.gitSigned(ctx, repository, "commit", "-S", "-m", message); err != nil {
		return err
	}
	if scope == sharedLedger {
		_, err := runtime.gitRun(ctx, repository, "push", "-q", "origin", "main")
		return err
	}
	return nil
}

func (runtime *Runtime) signRecord(ctx context.Context, path string) error {
	os.Remove(path + ".sig")
	if _, err := runtime.run(ctx, runtime.sshKeygen,
		[]string{"-Y", "sign", "-q", "-f", runtime.signingKey, "-n", "subyard-keys", path}, nil, nil); err != nil {
		return err
	}
	info, err := os.Stat(path + ".sig")
	if err != nil || info.Size() == 0 {
		return errors.New("could not sign credential revision")
	}
	return nil
}

func (runtime *Runtime) verifyRecord(ctx context.Context, root, path string) error {
	metadata, err := runtime.readRecordMetadata(path)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative != filepath.Join("records", metadata.CredentialID, metadata.RevisionID+".json") {
		return errors.New("credential record path is invalid")
	}
	expected, err := runtime.ageRecipients(metadata.RecipientActors)
	if err != nil {
		return err
	}
	envelope, err := readRecord(path)
	if err != nil {
		return err
	}
	var sops struct {
		Age []struct {
			Recipient string `json:"recipient"`
		} `json:"age"`
	}
	if err := json.Unmarshal(envelope.SOPS, &sops); err != nil {
		return err
	}
	actual := make([]string, 0, len(sops.Age))
	for _, item := range sops.Age {
		actual = append(actual, item.Recipient)
	}
	slices.Sort(actual)
	actual = slices.Compact(actual)
	slices.Sort(expected)
	expected = slices.Compact(expected)
	if !slices.Equal(actual, expected) {
		return errors.New("encrypted record recipients do not match signed metadata")
	}
	signature := path + ".sig"
	if info, err := os.Stat(signature); err != nil || info.Size() == 0 {
		return errors.New("credential record signature is missing")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = runtime.run(ctx, runtime.sshKeygen,
		[]string{"-Y", "verify", "-q", "-f", runtime.allowedSigners, "-I", metadata.ActorID,
			"-n", "subyard-keys", "-s", signature}, file, nil)
	return err
}

func (runtime *Runtime) decrypt(ctx context.Context, scope ledgerScope, metadata domain.CredentialMetadata) ([]byte, error) {
	path := runtime.recordPath(scope, metadata.CredentialID, metadata.RevisionID)
	return runtime.decryptPath(ctx, runtime.repository(scope), path)
}

func (runtime *Runtime) decryptPath(ctx context.Context, root, path string) ([]byte, error) {
	if err := runtime.verifyRecord(ctx, root, path); err != nil {
		return nil, err
	}
	output, err := runtime.run(ctx, runtime.sops,
		[]string{"decrypt", "--input-type", "json", "--output-type", "json", path}, nil,
		map[string]string{"SOPS_AGE_KEY_FILE": runtime.ageIdentity})
	if err != nil {
		return nil, err
	}
	defer clear(output)
	var envelope struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		return nil, err
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return nil, err
	}
	if len(payload) > maximumPayload {
		clear(payload)
		return nil, errors.New("credential payload exceeds size limit")
	}
	return payload, nil
}

func (runtime *Runtime) ageRecipients(actors []string) ([]string, error) {
	result := make([]string, 0, len(actors))
	for _, actor := range actors {
		recipient, err := runtime.recipientForActor(actor)
		if err != nil {
			return nil, err
		}
		result = append(result, recipient)
	}
	result = compactSorted(result)
	if len(result) == 0 {
		return nil, errors.New("a revision must have at least one age recipient")
	}
	return result, nil
}

func (runtime *Runtime) capturePayload(source string) ([]byte, error) {
	var reader io.Reader = runtime.config.Stdin
	if source != "" {
		real, err := runtime.validateImportPath(source)
		if err != nil {
			return nil, err
		}
		file, err := os.Open(real)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		reader = file
	} else if file, ok := runtime.config.Stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		fmt.Fprint(runtime.config.Stderr, "Secret value (input hidden): ")
		payload, err := term.ReadPassword(int(file.Fd()))
		fmt.Fprintln(runtime.config.Stderr)
		if err != nil {
			return nil, err
		}
		if len(payload) == 0 {
			return nil, errors.New("secret value is empty")
		}
		return payload, nil
	}
	payload, err := io.ReadAll(io.LimitReader(reader, maximumPayload+1))
	if err != nil {
		clear(payload)
		return nil, err
	}
	if len(payload) == 0 {
		return nil, errors.New("secret stdin was empty")
	}
	if len(payload) > maximumPayload {
		clear(payload)
		return nil, errors.New("credential payload exceeds size limit")
	}
	return payload, nil
}

func (runtime *Runtime) rejectProductionPayload(payload []byte) error {
	denylist := runtime.env["SUBYARD_KEYS_PROD_FINGERPRINTS"]
	if denylist == "" {
		denylist = filepath.Join(filepath.Dir(runtime.config.ConsumerRoot),
			"overrides", "host", "prod-fingerprints")
	}
	denied, err := readFingerprints(denylist)
	if err != nil {
		return err
	}
	if len(denied) == 0 {
		return nil
	}
	if fingerprintDenied(payload, denied) {
		return errors.New("credential payload matches a recorded production fingerprint; refusing import")
	}
	for _, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		_, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' ||
			value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		if fingerprintDenied([]byte(value), denied) {
			return errors.New("credential payload contains a value matching a recorded production fingerprint; refusing import")
		}
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if decoder.Decode(&value) == nil {
		if containsDeniedJSON(value, denied) {
			return errors.New("credential payload contains a value matching a recorded production fingerprint; refusing import")
		}
	}
	return nil
}

func (runtime *Runtime) validateImportPath(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("credential source is not a regular file: %s", path)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("credential import refuses non-regular files and symlinks: %s", path)
	}
	if info.Mode().Perm() != 0o600 && info.Mode().Perm() != 0o400 {
		return "", fmt.Errorf("credential source must have mode 0600 or 0400: %s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("credential source must be owned by the current user: %s", path)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	realInfo, err := os.Lstat(real)
	if err != nil || !realInfo.Mode().IsRegular() || realInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(info, realInfo) {
		return "", fmt.Errorf("credential source changed during metadata validation: %s", path)
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return "", err
	}
	clean := filepath.ToSlash(real)
	if strings.Contains(clean, "/.codex/") || strings.Contains(clean, "/.claude/") ||
		strings.Contains(clean, "/.pi/") || strings.Contains(clean, "/.config/opencode/") ||
		strings.HasSuffix(clean, "/auth.json") || strings.HasSuffix(clean, "/credentials/oauth.json") ||
		strings.Contains(clean, "/srv/staging/") && strings.Contains(clean, "/creds/") {
		return "", fmt.Errorf("mutable coding-agent/OAuth stores cannot be imported into the credential ledger: %s", path)
	}
	return real, nil
}

func readFingerprints(path string) (map[string]struct{}, error) {
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := make(map[string]struct{})
	for _, line := range strings.Split(string(payload), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || len(fields[0]) != sha256.Size*2 {
			continue
		}
		if _, err := hex.DecodeString(fields[0]); err == nil {
			result[strings.ToLower(fields[0])] = struct{}{}
		}
	}
	return result, nil
}

func fingerprintDenied(payload []byte, denied map[string]struct{}) bool {
	digest := sha256.Sum256(payload)
	_, found := denied[hex.EncodeToString(digest[:])]
	return found
}

func containsDeniedJSON(value any, denied map[string]struct{}) bool {
	switch typed := value.(type) {
	case string:
		return fingerprintDenied([]byte(typed), denied)
	case []any:
		for _, item := range typed {
			if containsDeniedJSON(item, denied) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if containsDeniedJSON(item, denied) {
				return true
			}
		}
	}
	return false
}

func validCredentialID(value string) bool {
	if len(value) != len("cred-")+32 || !strings.HasPrefix(value, "cred-") {
		return false
	}
	_, err := hex.DecodeString(value[len("cred-"):])
	return err == nil && strings.ToLower(value) == value
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func compactSorted(values []string) []string {
	slices.Sort(values)
	return slices.Compact(values)
}
