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

// nmDeviceGuardMigration marks the power-metadata reader boundary of the [device-subyard]
// NetworkManager guard; the host-network-guard activation owner rewrites the file itself.
const nmDeviceGuardMigration = "nm-device-guard-v1"

func preRegisteredYardsCapabilityCatalog() CapabilityCatalog {
	previous := PreviousBuiltinCapabilityCatalog()
	catalog, err := NewCapabilityCatalog(append(slices.Clone(previous.descriptors),
		CapabilityDescriptor{Kind: "local-ssh-port-settings-v2-to-v3", Domain: "settings", Version: 1},
		// Epoch-1 owners cannot honor detached optional listener state.
		// No backward-compatible policy means their rollback is forbidden.
		CapabilityDescriptor{Kind: "owner-publication-state-v1", Domain: "power-metadata", Version: 1},
	))
	if err != nil {
		panic(err)
	}
	return catalog
}

func registeredYardsCapabilityCatalog() CapabilityCatalog {
	previous := preRegisteredYardsCapabilityCatalog()
	catalog, err := NewCapabilityCatalog(append(slices.Clone(previous.descriptors),
		CapabilityDescriptor{Kind: registeredYardsMigration, Domain: "owner-registration", Version: 1},
		CapabilityDescriptor{Kind: guestDiskTmpMigration, Domain: "power-metadata", Version: 1},
	))
	if err != nil {
		panic(err)
	}
	return catalog
}

func BuiltinCapabilityCatalog() CapabilityCatalog {
	previous := registeredYardsCapabilityCatalog()
	catalog, err := NewCapabilityCatalog(append(slices.Clone(previous.descriptors),
		// Released power readers reject the [device-subyard] NetworkManager guard and their init
		// rewrites the released guard. No backward-compatible policy forbids rollback to them.
		CapabilityDescriptor{Kind: nmDeviceGuardMigration, Domain: "power-metadata", Version: 1},
	))
	if err != nil {
		panic(err)
	}
	return catalog
}

// Retained sealed owners keep their exact compiled catalog identity.
func PreviousBuiltinCapabilityCatalog() CapabilityCatalog {
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

func SupportedBuiltinCatalog(digest Fingerprint) (CapabilityCatalog, bool) {
	current, previous := BuiltinCapabilityCatalog(), PreviousBuiltinCapabilityCatalog()
	catalogs := []CapabilityCatalog{current, previous, preRegisteredYardsCapabilityCatalog(), registeredYardsCapabilityCatalog()}
	// Both independently introduced implementations may own sealed journals.
	// Recognize only their exact compiled catalogs, never arbitrary subsets.
	for _, kind := range []string{"local-ssh-port-settings-v2-to-v3", "owner-publication-state-v1"} {
		descriptor, _ := current.descriptor(kind)
		catalog, err := NewCapabilityCatalog(append(slices.Clone(previous.descriptors), descriptor))
		if err != nil {
			return CapabilityCatalog{}, false
		}
		catalogs = append(catalogs, catalog)
	}
	for _, catalog := range catalogs {
		if catalog.Digest() == digest {
			return catalog, true
		}
	}
	return CapabilityCatalog{}, false
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

// Descriptors returns the immutable compiled catalog as delivery facts.
func (catalog CapabilityCatalog) Descriptors() []CapabilityDescriptor {
	return slices.Clone(catalog.descriptors)
}

// SupportsSourceMetadata admits read-only history from exact supported catalogs
// with unchanged compiled semantics. The registry stays within its owner's catalog.
func (catalog CapabilityCatalog) SupportsSourceMetadata(source Fingerprint, registry RegistryV2) bool {
	if source == catalog.Digest() {
		return registry.Validate(catalog) == nil
	}
	historical, supported := SupportedBuiltinCatalog(source)
	if !supported {
		return false
	}
	for _, descriptor := range historical.descriptors {
		current, exists := catalog.descriptor(descriptor.Kind)
		if !exists || current != descriptor {
			return false
		}
	}
	return registry.Validate(historical) == nil
}

func fingerprintPayload(payload []byte) Fingerprint {
	digest := sha256.Sum256(payload)
	return Fingerprint(hex.EncodeToString(digest[:]))
}
