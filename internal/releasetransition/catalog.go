package releasetransition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/blang/semver/v4"
)

// CapabilityDescriptor identifies one compiled migration implementation. Its
// canonical policy, bounds, and recovery semantics are part of the catalog
// digest and cannot be supplied or changed by registry data.
type CapabilityDescriptor struct {
	Kind                  string                `json:"kind"`
	Domain                string                `json:"domain"`
	Version               int                   `json:"version"`
	RollbackCompatibility RollbackCompatibility `json:"rollbackCompatibility,omitempty"`
	LegacyMinimumVersion  string                `json:"legacyMinimumVersion,omitempty"`
}

type RollbackCompatibility string

const RollbackCompatibilityBackwardCompatibleNoOp RollbackCompatibility = "backward-compatible-no-op"

// CapabilityCatalog is an immutable allowlist of compiled implementations.
// Registry data may select a kind, but cannot supply or alter its semantics.
type CapabilityCatalog struct {
	descriptors []CapabilityDescriptor
	byKind      map[string]CapabilityDescriptor
	digest      Fingerprint
}

func NewCapabilityCatalog(descriptors []CapabilityDescriptor) (CapabilityCatalog, error) {
	canonical := slices.Clone(descriptors)
	slices.SortFunc(canonical, func(left, right CapabilityDescriptor) int {
		if left.Kind < right.Kind {
			return -1
		}
		if left.Kind > right.Kind {
			return 1
		}
		return 0
	})
	byKind := make(map[string]CapabilityDescriptor, len(canonical))
	for _, descriptor := range canonical {
		if err := validateSafeID(descriptor.Kind, "capability kind"); err != nil {
			return CapabilityCatalog{}, err
		}
		if err := validateSafeID(descriptor.Domain, "capability domain"); err != nil {
			return CapabilityCatalog{}, err
		}
		if descriptor.Version < 1 {
			return CapabilityCatalog{}, invalid("capability %q has invalid version", descriptor.Kind)
		}
		switch descriptor.RollbackCompatibility {
		case "":
			if descriptor.LegacyMinimumVersion != "" {
				return CapabilityCatalog{}, invalid("capability %q has incomplete rollback policy", descriptor.Kind)
			}
		case RollbackCompatibilityBackwardCompatibleNoOp:
			minimum, err := semver.Parse(descriptor.LegacyMinimumVersion)
			if err != nil || minimum.String() != descriptor.LegacyMinimumVersion {
				return CapabilityCatalog{}, invalid("capability %q has invalid legacy minimum version", descriptor.Kind)
			}
		default:
			return CapabilityCatalog{}, invalid("capability %q has unknown rollback policy", descriptor.Kind)
		}
		if _, exists := byKind[descriptor.Kind]; exists {
			return CapabilityCatalog{}, invalid("duplicate capability kind %q", descriptor.Kind)
		}
		byKind[descriptor.Kind] = descriptor
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return CapabilityCatalog{}, err
	}
	return CapabilityCatalog{
		descriptors: canonical,
		byKind:      byKind,
		digest:      fingerprintPayload(payload),
	}, nil
}

func BuiltinCapabilityCatalog() CapabilityCatalog {
	catalog, err := NewCapabilityCatalog([]CapabilityDescriptor{
		{
			Kind: "test-vms-settings-v1-to-v2", Domain: "settings", Version: 1,
			RollbackCompatibility: RollbackCompatibilityBackwardCompatibleNoOp,
			LegacyMinimumVersion:  "0.8.0",
		},
		{
			Kind: "test-yard-owner-v1-to-v2", Domain: "owner-registration", Version: 1,
			RollbackCompatibility: RollbackCompatibilityBackwardCompatibleNoOp,
			LegacyMinimumVersion:  "0.8.0",
		},
	})
	if err != nil {
		panic(err)
	}
	return catalog
}

func (catalog CapabilityCatalog) descriptor(kind string) (CapabilityDescriptor, bool) {
	descriptor, exists := catalog.byKind[kind]
	return descriptor, exists
}

func (catalog CapabilityCatalog) Supports(kind, domain string) bool {
	descriptor, exists := catalog.byKind[kind]
	return exists && descriptor.Domain == domain
}

func (catalog CapabilityCatalog) Digest() Fingerprint {
	return catalog.digest
}

func (catalog CapabilityCatalog) supportsV0111PostActivationRecovery(source Fingerprint) bool {
	if source == catalog.Digest() {
		return true
	}
	// Published v0.11.1 bound only kind/domain/version. Its exact migration
	// implementations are compatible when only rollback metadata was added.
	const publishedV0111 Fingerprint = "49e4c86efea0f1be557a43569728d9eb7f9df519379b0bdf45edd91ac5c93cf2"
	if source != publishedV0111 {
		return false
	}
	descriptors := slices.Clone(catalog.descriptors)
	for index := range descriptors {
		descriptors[index].RollbackCompatibility = ""
		descriptors[index].LegacyMinimumVersion = ""
	}
	historical, err := NewCapabilityCatalog(descriptors)
	return err == nil && historical.Digest() == publishedV0111
}

func fingerprintPayload(payload []byte) Fingerprint {
	digest := sha256.Sum256(payload)
	return Fingerprint(hex.EncodeToString(digest[:]))
}
