package credentialruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/credential"
	"github.com/Subyard/Subyard/internal/domain"
)

// ConsumerCredentialID inspects ledger metadata only. Ambiguous heads must be resolved by
// the operator; onboarding must never replace or resurrect a credential.
func (runtime *Runtime) ConsumerCredentialID(ctx context.Context, consumer, zone string) (string, error) {
	if _, err := runtime.ConsumerPath(consumer, zone); err != nil {
		return "", err
	}
	if !runtime.Initialized() {
		return "", nil
	}
	all, err := runtime.allRecords(ctx)
	if err != nil {
		return "", err
	}
	id := ""
	for _, scope := range []ledgerScope{sharedLedger, localLedger} {
		for _, candidate := range credentialIDs(all[scope]) {
			heads := credential.Heads(all[scope], candidate)
			for _, head := range heads {
				if head.Consumer != consumer || head.Zone != zone {
					continue
				}
				if len(heads) != 1 {
					return "", errors.New("profile credential has conflicting revisions; resolve them with yard keys resolve")
				}
				if head.State != "active" {
					continue
				}
				if id != "" {
					return "", errors.New("multiple active profile credentials; resolve the ledger before setup")
				}
				id = candidate
			}
		}
	}
	return id, nil
}

// PrepareSetupCredential plans a bounded import or recovery of the existing consumer.
// Preparation reads metadata only and also works before init creates the ledger.
type SetupCredentialOptions struct {
	Consumer, Zone, Label, Source string
	Settings                      map[string]any
}

func (runtime *Runtime) PrepareSetupCredential(ctx context.Context, options SetupCredentialOptions) (Prepared, error) {
	if options.Settings != nil {
		owner := runtime.consumerOwners[options.Consumer]
		if owner.Setup == nil || !owner.Setup.SyncFields {
			return Prepared{}, errors.New("consumer does not synchronize setup fields")
		}
		fields, err := owner.Setup.SharedSettings(options.Settings)
		if err != nil {
			return Prepared{}, err
		}
		options.Settings = fields
	}
	source := options.Source
	if err := validateClassification(options.Label, "file", options.Zone, options.Consumer); err != nil {
		return Prepared{}, err
	}
	expected, err := runtime.ConsumerCredentialID(ctx, options.Consumer, options.Zone)
	if err != nil {
		return Prepared{}, err
	}
	destination, mapped, err := runtime.consumerPath(options.Consumer, options.Zone)
	if err != nil {
		return Prepared{}, err
	}
	if !mapped {
		return Prepared{}, errors.New("setup requires a mapped credential consumer")
	}
	targetBefore, err := os.Lstat(destination)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Prepared{}, err
	}
	if targetBefore != nil && !targetBefore.Mode().IsRegular() {
		return Prepared{}, errors.New("profile credential consumer is not a regular file")
	}
	legacyPath := runtime.LegacyConsumerPath(options.Consumer)
	legacyBefore, err := protectedFileMetadata(legacyPath)
	if err != nil {
		return Prepared{}, err
	}
	expectedRevision := ""
	if expected != "" {
		_, head, err := runtime.singleHead(ctx, expected)
		if err != nil {
			return Prepared{}, err
		}
		expectedRevision = head.RevisionID
	}
	var before os.FileInfo
	if expected == "" {
		if source == "" {
			return Prepared{}, errors.New("credential file path is required")
		}
		source, err = runtime.validateImportSource(source, false)
		if err != nil {
			return Prepared{}, err
		}
		before, err = inspectSetupSource(source)
		if err != nil {
			return Prepared{}, err
		}
	} else if source != "" {
		return Prepared{}, errors.New("profile credential already exists; use yard keys rotate to replace it")
	}
	consequences := []string{"materialize the existing profile credential on this owner host"}
	if expected == "" {
		consequences = []string{
			"protect the selected file with mode 0600 and import it into the encrypted profile credential ledger",
			"allow the key to sync to trusted credential peers; keep the original file on this host",
			"materialize the profile credential on this owner host only",
		}
	}
	return runtime.mutation("keys.import", true, consequences, func(ctx context.Context) error {
		if err := runtime.requireInitialized(); err != nil {
			return err
		}
		return runtime.withLock(ctx, func() error {
			legacyNow, err := protectedFileMetadata(legacyPath)
			if err != nil || legacyNow != legacyBefore {
				return fmt.Errorf("%w: legacy profile credential changed; rerun init", domain.ErrPlanStale)
			}
			targetNow, err := os.Lstat(destination)
			if targetBefore == nil {
				if !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("%w: profile credential consumer appeared; rerun init", domain.ErrPlanStale)
				}
			} else if err != nil || !sameSetupSource(targetBefore, targetNow) {
				return fmt.Errorf("%w: profile credential consumer changed; rerun init", domain.ErrPlanStale)
			}
			current, err := runtime.ConsumerCredentialID(ctx, options.Consumer, options.Zone)
			if err != nil {
				return err
			}
			if current != expected {
				return fmt.Errorf("%w: profile credential changed; rerun init", domain.ErrPlanStale)
			}
			if current != "" {
				_, head, err := runtime.singleHead(ctx, current)
				if err != nil {
					return err
				}
				if head.RevisionID != expectedRevision {
					return fmt.Errorf("%w: profile credential revision changed; rerun init", domain.ErrPlanStale)
				}
			}
			if current == "" {
				// Pin the approved inode before tightening permissions or reading it.
				fd, err := syscall.Open(source, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
				if err != nil {
					return errors.New("cannot open selected credential file")
				}
				file := os.NewFile(uintptr(fd), source)
				defer file.Close()
				now, err := file.Stat()
				if err != nil || !sameSetupSource(before, now) {
					return fmt.Errorf("%w: selected credential file changed", domain.ErrPlanStale)
				}
				if now.Mode().Perm() != 0o400 {
					if err := file.Chmod(0o600); err != nil {
						return err
					}
				}
				// Retain the normal import path/ownership/OAuth-store restrictions.
				if _, err := runtime.validateImportPath(source); err != nil {
					return err
				}
				payload, err := io.ReadAll(io.LimitReader(file, maximumPayload+1))
				defer clear(payload)
				if err != nil {
					return errors.New("cannot read selected credential file")
				}
				if err := runtime.validateConsumerPayload(options.Consumer, payload); err != nil {
					return err
				}
				if err := runtime.rejectProductionPayload(payload); err != nil {
					return err
				}
				if options.Settings != nil {
					payload, _, err = runtime.withSetupSettings(options.Consumer, options.Settings, payload)
					defer clear(payload)
					if err != nil {
						return err
					}
				}
				current, err = runtime.add(ctx, addOptions{label: options.Label, kind: "file", zone: options.Zone, consumer: options.Consumer}, payload)
				if err != nil {
					return err
				}
			}
			if expected != "" && options.Settings != nil {
				scope, head, err := runtime.singleHead(ctx, current)
				if err != nil {
					return err
				}
				payload, err := runtime.decrypt(ctx, scope, head)
				if err != nil {
					return err
				}
				defer clear(payload)
				if !bytes.HasPrefix(bytes.TrimSpace(payload), []byte("{")) {
					localPath := ""
					if targetBefore != nil {
						localPath = destination
					} else if legacyBefore.Exists {
						localPath = legacyPath
					}
					if localPath != "" {
						local, err := runtime.readConsumerFile(options.Consumer, options.Zone, localPath)
						if err != nil {
							return err
						}
						agrees := bytes.Equal(bytes.TrimSpace(local), bytes.TrimSpace(payload))
						clear(local)
						if !agrees {
							return errors.New("local profile key conflicts with the ledger; existing connection was kept")
						}
					}
				}
				bundle, changed, err := runtime.withSetupSettings(options.Consumer, options.Settings, payload)
				defer clear(bundle)
				if err != nil {
					return err
				}
				if changed {
					spec := specFromMetadata(head)
					spec.Parents = []string{head.RevisionID}
					if _, err := runtime.publish(ctx, scope, spec, bundle); err != nil {
						return err
					}
				}
			}
			scope, err := runtime.findScope(current)
			if err != nil {
				return err
			}
			return runtime.materializeCredential(ctx, scope, current, false)
		})
	}), nil
}

func (runtime *Runtime) withSetupSettings(consumer string, settings map[string]any, payload []byte) ([]byte, bool, error) {
	schema := runtime.consumerOwners[consumer].Setup
	if !bytes.HasPrefix(bytes.TrimSpace(payload), []byte("{")) {
		bundle, err := schema.EncodeCredentialSettings(settings, payload)
		return bundle, true, err
	}
	bundle, err := schema.DecodeCredentialSettings(payload)
	if err != nil {
		return nil, false, err
	}
	want, err := schema.SharedSettings(settings)
	if err != nil {
		return nil, false, err
	}
	a, _ := json.Marshal(bundle.Settings)
	b, _ := json.Marshal(want)
	if !bytes.Equal(a, b) {
		return nil, false, errors.New("local profile settings conflict with the synchronized connection; existing settings were kept")
	}
	return payload, false, nil
}

// ConsumerFileBinding captures only metadata for init's local settings adoption.
func (runtime *Runtime) ConsumerFileBinding(path string) (string, error) {
	fact, err := protectedFileMetadata(path)
	return metadataDigest(fact), err
}

func inspectSetupSource(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("credential file must be an existing file on the owner host")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return nil, errors.New("credential file must be an operator-owned regular file, not a link")
	}
	if info.Size() == 0 || info.Size() > maximumPayload {
		return nil, errors.New("credential file has an invalid size")
	}
	return info, nil
}

func sameSetupSource(before, now os.FileInfo) bool {
	if before == nil || now == nil || !os.SameFile(before, now) {
		return false
	}
	a, aok := before.Sys().(*syscall.Stat_t)
	b, bok := now.Sys().(*syscall.Stat_t)
	return aok && bok && a.Mode == b.Mode && a.Uid == b.Uid && a.Gid == b.Gid &&
		a.Nlink == b.Nlink && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}

// ConsumerPath resolves a declared consumer without reading credential contents.
func (runtime *Runtime) ConsumerPath(consumer, zone string) (string, error) {
	path, mapped, err := runtime.consumerPath(consumer, zone)
	if err != nil {
		return "", err
	}
	if !mapped {
		return "", errors.New("credential consumer has no materialization path")
	}
	return path, nil
}

func (runtime *Runtime) validateConsumerPayload(id string, payload []byte) error {
	for _, consumer := range runtime.consumers {
		if consumer.ID != id {
			continue
		}
		switch consumer.Format {
		case "rsa-private-key-with-settings":
			if !bytes.HasPrefix(bytes.TrimSpace(payload), []byte("{")) {
				return credential.ValidateRSAPrivateKey(payload)
			}
			bundle, err := runtime.consumerOwners[id].Setup.DecodeCredentialSettings(payload)
			if err != nil {
				return err
			}
			return credential.ValidateRSAPrivateKey([]byte(bundle.PrivateKey))
		case "rsa-private-key":
			return credential.ValidateRSAPrivateKey(payload)
		case "file":
			return nil
		default:
			return errors.New("unsupported credential consumer format")
		}
	}
	return nil
}

func (runtime *Runtime) preserveCredentialSettings(ctx context.Context, scope ledgerScope, head domain.CredentialMetadata, replacement []byte) ([]byte, error) {
	owner := runtime.consumerOwners[head.Consumer]
	if owner.Setup == nil || !owner.Setup.SyncFields || bytes.HasPrefix(bytes.TrimSpace(replacement), []byte("{")) {
		return replacement, nil
	}
	previous, err := runtime.decrypt(ctx, scope, head)
	if err != nil {
		return nil, err
	}
	defer clear(previous)
	if !bytes.HasPrefix(bytes.TrimSpace(previous), []byte("{")) {
		return replacement, nil
	}
	bundle, err := owner.Setup.DecodeCredentialSettings(previous)
	if err != nil {
		return nil, err
	}
	return owner.Setup.EncodeCredentialSettings(bundle.Settings, replacement)
}

// Selecting or rotating a legacy key retains fields only when active bundle
// heads agree. Choosing a complete bundle explicitly selects its entire value.
func (runtime *Runtime) preserveResolvedSettings(ctx context.Context, scope ledgerScope, heads []domain.CredentialMetadata, replacement []byte) ([]byte, error) {
	if bytes.HasPrefix(bytes.TrimSpace(replacement), []byte("{")) {
		return replacement, nil
	}
	var preserved []byte
	for _, head := range heads {
		if head.State != "active" {
			continue
		}
		next, err := runtime.preserveCredentialSettings(ctx, scope, head, replacement)
		if err != nil {
			clear(preserved)
			return nil, err
		}
		if bytes.Equal(next, replacement) {
			continue
		}
		if preserved != nil && !bytes.Equal(preserved, next) {
			clear(next)
			clear(preserved)
			return nil, errors.New("conflicting connection fields require choosing or supplying a complete bundle")
		}
		if preserved == nil {
			preserved = next
		} else {
			clear(next)
		}
	}
	if preserved == nil {
		return replacement, nil
	}
	return preserved, nil
}

// ValidateConsumerFile checks a protected, pinned file against its declared format.
// It does not return file contents or permit undeclared consumers.
func (runtime *Runtime) ValidateConsumerFile(consumer, zone, path string) error {
	payload, err := runtime.readConsumerFile(consumer, zone, path)
	defer clear(payload)
	return err
}

// ConsumerSettings returns only declared fields from an atomically materialized bundle.
// A legacy PEM has no shared fields yet.
func (runtime *Runtime) ConsumerSettings(consumer, zone, path string) (map[string]any, error) {
	payload, err := runtime.readConsumerFile(consumer, zone, path)
	defer clear(payload)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(bytes.TrimSpace(payload), []byte("{")) {
		return nil, nil
	}
	owner := runtime.consumerOwners[consumer]
	if owner.Setup == nil || !owner.Setup.SyncFields {
		return nil, nil
	}
	bundle, err := owner.Setup.DecodeCredentialSettings(payload)
	return bundle.Settings, err
}

func (runtime *Runtime) LegacyConsumerPath(consumer string) string {
	for _, item := range runtime.consumers {
		if item.ID == consumer && item.LegacyPath != "" {
			return filepath.Join(runtime.config.ConsumerRoot, item.LegacyPath)
		}
	}
	return ""
}

func (runtime *Runtime) removeLegacyConsumer(consumer, zone string) error {
	path := runtime.LegacyConsumerPath(consumer)
	if path == "" {
		return nil
	}
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := runtime.ValidateConsumerFile(consumer, zone, path); err != nil {
		return err
	}
	now, err := os.Lstat(path)
	if err != nil || !sameSetupSource(before, now) {
		return fmt.Errorf("%w: legacy credential changed during migration", domain.ErrPlanStale)
	}
	return os.Remove(path)
}

func (runtime *Runtime) readConsumerFile(consumer, zone, path string) ([]byte, error) {
	if _, err := runtime.ConsumerPath(consumer, zone); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") {
		return nil, errors.New("credential file path is invalid")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("cannot open protected credential file")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("credential file must be regular")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || (info.Mode().Perm() != 0o600 && info.Mode().Perm() != 0o400) {
		return nil, errors.New("credential file ownership or permissions are unsafe")
	}
	if info.Size() <= 0 || info.Size() > maximumPayload {
		return nil, errors.New("credential file has an invalid size")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maximumPayload+1))
	if err != nil || len(payload) == 0 || len(payload) > maximumPayload {
		clear(payload)
		return nil, errors.New("cannot read bounded credential file")
	}
	now, err := file.Stat()
	if err != nil || !sameSetupSource(info, now) {
		clear(payload)
		return nil, errors.New("credential file changed during validation")
	}
	if err := runtime.validateConsumerPayload(consumer, payload); err != nil {
		clear(payload)
		return nil, err
	}
	return payload, nil
}
