package credentialruntime

import (
	"context"
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
}

func (runtime *Runtime) PrepareSetupCredential(ctx context.Context, options SetupCredentialOptions) (Prepared, error) {
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
				current, err = runtime.add(ctx, addOptions{label: options.Label, kind: "file", zone: options.Zone, consumer: options.Consumer}, payload)
				if err != nil {
					return err
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

// ValidateConsumerFile checks a protected, pinned file against its declared format.
// It does not return file contents or permit undeclared consumers.
func (runtime *Runtime) ValidateConsumerFile(consumer, zone, path string) error {
	if _, err := runtime.ConsumerPath(consumer, zone); err != nil {
		return err
	}
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") {
		return errors.New("credential file path is invalid")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("cannot open protected credential file")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("credential file must be regular")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || (info.Mode().Perm() != 0o600 && info.Mode().Perm() != 0o400) {
		return errors.New("credential file ownership or permissions are unsafe")
	}
	if info.Size() <= 0 || info.Size() > maximumPayload {
		return errors.New("credential file has an invalid size")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maximumPayload+1))
	defer clear(payload)
	if err != nil || len(payload) == 0 || len(payload) > maximumPayload {
		return errors.New("cannot read bounded credential file")
	}
	now, err := file.Stat()
	if err != nil || !sameSetupSource(info, now) {
		return errors.New("credential file changed during validation")
	}
	return runtime.validateConsumerPayload(consumer, payload)
}
