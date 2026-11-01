package ownerpublication

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// HandoffPrefix carries delivery acknowledgement only. Receipt v1 remains the
// complete listener authority and old engines safely ignore this separate key.
const HandoffPrefix = "user.subyard.publication_handoff."
const HandoffService = "subyard-publication-reconcile.service"

type Consumer struct {
	EngineDigest    string                           `json:"engineDigest"`
	UnitDigest      string                           `json:"unitDigest"`
	ContractsDigest string                           `json:"contractsDigest"`
	FilesDigest     string                           `json:"filesDigest"`
	Generations     []ports.NativeResourceGeneration `json:"generations"`
}
type Handoff struct {
	Version        int             `json:"version"`
	ID             string          `json:"id"`
	Invocation     string          `json:"invocation"`
	Intent         string          `json:"intent"`
	Recipe         string          `json:"recipe"`
	Project        string          `json:"project"`
	Instance       string          `json:"instance"`
	UUID           string          `json:"uuid"`
	Kind           domain.YardKind `json:"kind"`
	Scope          string          `json:"scope"`
	ReceiptKey     string          `json:"receiptKey"`
	Receipt        string          `json:"receipt"`
	ReceiptDigest  string          `json:"receiptDigest"`
	ContractDigest string          `json:"contractDigest"`
	Consumer       Consumer        `json:"consumer"`
	Ack            string          `json:"ack"`
}

func Digest(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }
func valueDigest(v any) string { raw, _ := json.Marshal(v); return Digest(raw) }
func hash(v string) bool       { return len(v) == 64 && strings.Trim(v, "0123456789abcdef") == "" }
func (c Consumer) Valid() bool {
	if !hash(c.EngineDigest) || !hash(c.UnitDigest) || !hash(c.ContractsDigest) || !hash(c.FilesDigest) || len(c.Generations) != 3 {
		return false
	}
	for i, r := range []string{"engine", "service", "publication"} {
		if c.Generations[i].Role != r || c.Generations[i].Value != "0" && !hash(c.Generations[i].Value) {
			return false
		}
	}
	return true
}
func (h Handoff) Key() string      { return HandoffPrefix + strings.TrimPrefix(h.ReceiptKey, Prefix) }
func (h Handoff) Encode() string   { raw, _ := json.Marshal(h); return string(raw) }
func (h Handoff) identity() string { h.ID = ""; h.Ack = ""; return valueDigest(h) }
func DecodeHandoff(raw string) (Handoff, error) {
	var h Handoff
	if len(raw) > 32<<10 || strictJSON([]byte(raw), &h) != nil || h.Encode() != raw || h.Version != 1 || !hash(h.ID) || h.ID != h.identity() || !hash(h.Invocation) || !hash(h.Intent) || !hash(h.Recipe) || !domain.SafeName(h.Project) || !domain.SafeName(h.Instance) || h.UUID == "" || (h.Kind != domain.YardVM && h.Kind != domain.YardContainer) || (h.Scope != "legacy" && h.Scope != "current" && h.Scope != "recreate") || !strings.HasPrefix(h.ReceiptKey, Prefix) || strings.TrimPrefix(h.ReceiptKey, Prefix) == "" || !hash(h.ReceiptDigest) || Digest([]byte(h.Receipt)) != h.ReceiptDigest || !hash(h.ContractDigest) || !h.Consumer.Valid() || h.Ack != "" && h.Ack != h.ID {
		return Handoff{}, domain.ErrPlanStale
	}
	return h, nil
}
func NewHandoff(target OriginalTarget, device, invocation, intent, recipe string, consumer Consumer, instance ports.InstanceInfo, contracts []Contract) (Handoff, error) {
	h := Handoff{Version: 1, Invocation: invocation, Intent: intent, Recipe: recipe, Project: target.Project, Instance: target.Name, UUID: target.UUID, Kind: target.Kind, Scope: target.Scope, ReceiptKey: Prefix + device, Consumer: consumer}
	if h.Scope == "" {
		h.Scope = "legacy"
	}
	h.Receipt = instance.LocalConfig[h.ReceiptKey]
	h.ReceiptDigest = Digest([]byte(h.Receipt))
	for _, c := range contracts {
		if c.Device == device {
			h.ContractDigest = valueDigest(c)
		}
	}
	h.ID = h.identity()
	if _, err := DecodeHandoff(h.Encode()); err != nil {
		return Handoff{}, err
	}
	return h, h.Check(instance, contracts, false)
}

// Check accepts an absent receipt only with the exact restored local AND
// expanded device plus its original ownership marker. Empty Deferred is no proof.
func (h Handoff) Check(instance ports.InstanceInfo, contracts []Contract, restored bool) error {
	if err := (OriginalTarget{Project: h.Project, Name: h.Instance, UUID: h.UUID, Desired: domain.PowerRunning, Kind: h.Kind, Scope: h.Scope}).Check(instance); err != nil {
		return err
	}
	if _, _, _, err := Plan(instance, contracts, nil); err != nil {
		return err
	}
	for _, c := range contracts {
		if Prefix+c.Device != h.ReceiptKey {
			continue
		}
		if c.CleanupOnly || valueDigest(c) != h.ContractDigest {
			return domain.ErrPlanStale
		}
		var r Receipt
		if strictJSON([]byte(h.Receipt), &r) != nil || r.Version != 1 || !c.Owns(r.Ownership, r.Device) || instance.LocalConfig[c.Key] != r.Ownership || instance.Config[c.Key] != r.Ownership {
			return domain.ErrPlanStale
		}
		if instance.LocalConfig[h.ReceiptKey] == h.Receipt && instance.Config[h.ReceiptKey] == h.Receipt {
			return nil
		}
		if restored && instance.LocalConfig[h.ReceiptKey] == "" && instance.Config[h.ReceiptKey] == "" && maps.Equal(instance.LocalDevices[c.Device], r.Device) && maps.Equal(instance.Devices[c.Device], r.Device) {
			return nil
		}
		return domain.ErrPlanStale
	}
	return domain.ErrPlanStale
}

// Deliver waits only for independent receipt delivery, never for an address.
// Every reissue names the same passive unit; systemd may coalesce active starts.
// The delivery budget covers an older 30s attempt and its 30s retry turnover.
func Deliver(ctx context.Context, expected []Handoff, inspect func(context.Context) (ports.InstanceInfo, error), consumer func(context.Context) error, exchange func(context.Context, Handoff, string, string) error, start func(context.Context) error) (result error) {
	if len(expected) == 0 {
		return domain.ErrPlanStale
	}
	first := expected[0]
	seen := map[string]bool{}
	for _, h := range expected {
		if h.Ack != "" || seen[h.Key()] {
			return domain.ErrPlanStale
		}
		seen[h.Key()] = true
		if _, err := DecodeHandoff(h.Encode()); err != nil {
			return err
		}
		if h.Project != first.Project || h.Instance != first.Instance || h.UUID != first.UUID || h.Kind != first.Kind || h.Invocation != first.Invocation || h.Intent != first.Intent || h.Recipe != first.Recipe || valueDigest(h.Consumer) != valueDigest(first.Consumer) {
			return domain.ErrPlanStale
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	defer func() {
		if result != nil && ctx.Err() != nil {
			if o := ports.NativeOperationObserverFrom(ctx); o != nil {
				o.Uncertain()
			}
		}
	}()
	if err := consumer(ctx); err != nil {
		return err
	}
	for _, h := range expected {
		if err := exchange(ctx, h, "", h.Encode()); err != nil {
			return err
		}
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("publication delivery unknown: %w", context.Cause(ctx))
		}
		if err := consumer(ctx); err != nil {
			return err
		}
		if err := start(ctx); err != nil {
			return err
		}
		i, err := inspect(ctx)
		if err != nil {
			return err
		}
		acknowledged := make([]string, len(expected))
		all := true
		for index, h := range expected {
			current, err := DecodeHandoff(i.LocalConfig[h.Key()])
			if err != nil || current.ID != h.ID {
				return domain.ErrPlanStale
			}
			if current.Ack != h.ID {
				all = false
			} else {
				acknowledged[index] = current.Encode()
			}
		}
		if all {
			if err := consumer(ctx); err != nil {
				return err
			}
			for index, h := range expected {
				if err := exchange(ctx, h, acknowledged[index], ""); err != nil {
					return err
				}
			}
			if _, err := inspect(ctx); err != nil {
				return err
			}
			return consumer(ctx)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("publication delivery unknown: %w", context.Cause(ctx))
		case <-ticker.C:
		}
	}
}

func CheckRestoredOriginal(current ports.InstanceInfo, original *ports.InstanceInfo, contracts []Contract) error {
	if original == nil {
		return nil
	} // Historical native records without handoff keep no new authority.
	for _, c := range contracts {
		before := *original
		if before.LocalConfig[Prefix+c.Device] == "" && len(before.LocalDevices[c.Device]) == 0 {
			continue
		}
		// Construct the exact original v1 receipt for comparison only. The durable
		// authority itself is still exclusively written by Plan.
		raw := before.LocalConfig[Prefix+c.Device]
		if raw == "" && c.Owns(before.LocalConfig[c.Key], before.LocalDevices[c.Device]) {
			payload, _ := json.Marshal(Receipt{Version: 1, Ownership: before.LocalConfig[c.Key], Device: before.LocalDevices[c.Device]})
			raw = string(payload)
		}
		if raw == "" {
			continue
		}
		var receipt Receipt
		if json.Unmarshal([]byte(raw), &receipt) != nil || c.CleanupOnly || current.LocalConfig[c.Key] != receipt.Ownership || current.Config[c.Key] != receipt.Ownership || !maps.Equal(current.LocalDevices[c.Device], receipt.Device) || !maps.Equal(current.Devices[c.Device], receipt.Device) {
			return domain.ErrPlanStale
		}
	}
	return nil
}

// PublicationCommand carries only installation paths already approved in the
// protected unit. Base64 keeps positional data independent of systemd quoting.
func PublicationCommand(engine, unit string) string {
	return systemdExecutable(engine) + " _publication-reconcile " + base64.StdEncoding.EncodeToString([]byte(engine)) + " " + base64.StdEncoding.EncodeToString([]byte(unit))
}
func systemdExecutable(path string) string {
	var b strings.Builder
	b.WriteString(":\"")
	for _, c := range []byte(path) {
		switch c {
		case '%':
			b.WriteByte(c)
			b.WriteByte(c)
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			if c < 32 || c >= 127 {
				fmt.Fprintf(&b, "\\x%02x", c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
func PublicationPaths(args []string) (string, string, error) {
	if len(args) != 2 {
		return "", "", domain.ErrPlanStale
	}
	paths := make([]string, 2)
	for i, arg := range args {
		raw, err := base64.StdEncoding.Strict().DecodeString(arg)
		if err != nil || base64.StdEncoding.EncodeToString(raw) != arg || len(raw) == 0 || strings.ContainsRune(string(raw), 0) || !filepath.IsAbs(string(raw)) {
			return "", "", domain.ErrPlanStale
		}
		paths[i] = string(raw)
	}
	if err := ValidatePublicationPaths(paths[0], paths[1]); err != nil {
		return "", "", err
	}
	return paths[0], paths[1], nil
}
func RenderPublicationUnit(template, engine, unit, contracts string) string {
	return strings.ReplaceAll(template, "@SUBYARD_PUBLICATION_COMMAND@", PublicationCommand(engine, unit)) + "\n[Service]\nEnvironment=SUBYARD_PUBLICATION_CONTRACTS=" + contracts + "\n"
}

// systemd refuses these executable bytes even when C-escaped. Refuse before
// consent instead of installing an unstartable delivery consumer.
func ValidatePublicationPaths(engine, unit string) error {
	if !filepath.IsAbs(engine) || !filepath.IsAbs(unit) || !utf8.ValidString(engine) || strings.ContainsAny(engine, "\\\"'") {
		return domain.ErrPlanStale
	}
	for _, c := range []byte(engine) {
		if c < 32 || c == 127 {
			return domain.ErrPlanStale
		}
	}
	return nil
}
