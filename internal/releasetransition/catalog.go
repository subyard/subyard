package releasetransition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
)

// CapabilityDescriptor identifies one compiled migration implementation. The
// implementation owns its policy, bounds, and recovery semantics; changing any
// of them requires a version bump here so the catalog digest changes as well.
type CapabilityDescriptor struct {
	Kind    string `json:"kind"`
	Domain  string `json:"domain"`
	Version int    `json:"version"`
}

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
		{Kind: "test-vms-settings-v1-to-v2", Domain: "settings", Version: 1},
		{Kind: "test-yard-owner-v1-to-v2", Domain: "owner-registration", Version: 1},
	})
	if err != nil {
		panic(err)
	}
	return catalog
}

func (catalog CapabilityCatalog) Supports(kind, domain string) bool {
	descriptor, exists := catalog.byKind[kind]
	return exists && descriptor.Domain == domain
}

func (catalog CapabilityCatalog) Digest() Fingerprint {
	return catalog.digest
}

func fingerprintPayload(payload []byte) Fingerprint {
	digest := sha256.Sum256(payload)
	return Fingerprint(hex.EncodeToString(digest[:]))
}
