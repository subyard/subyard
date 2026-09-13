package yardnetwork

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/Subyard/Subyard/internal/ports"
)

// Yard is a resolved local instance identity. CLI selectors never reach Incus.
type Yard struct {
	Name     string `json:"name"`
	Project  string `json:"project"`
	Instance string `json:"instance"`
	Network  string `json:"network"`
}

type StoredPolicy struct {
	Policy  Policy
	Content string
	ETag    string
}

type Rule struct {
	Action          string `json:"action"`
	State           string `json:"state"`
	Source          string `json:"source,omitempty"`
	Destination     string `json:"destination,omitempty"`
	Protocol        string `json:"protocol,omitempty"`
	DestinationPort string `json:"destinationPort,omitempty"`
}

type ACL struct {
	Name    string
	Exists  bool
	ETag    string
	Config  map[string]string
	Ingress []Rule
	Egress  []Rule
}

type ObservedYard struct {
	Yard
	ProjectFound   bool
	ProjectETag    string
	ProjectConfig  map[string]string
	ProfileFound   bool
	ProfileETag    string
	ProfileDevices map[string]map[string]string
	ProfileUsedBy  []string
	InstanceFound  bool
	InstanceInfo   ports.InstanceInfo
	IPv4           string
	MAC            string
	ACL            ACL
}

type Network struct {
	Name    string
	Type    string
	Managed bool
	Config  map[string]string
}

// ReservationClaim identifies the Incus object claiming a reserved address.
// Instance is preferred when the claim can be resolved exactly; Profile is set
// for an otherwise unowned static profile reservation.
type ReservationClaim struct {
	Project  string
	Instance string
	Profile  string
	MAC      string
}

type Snapshot struct {
	Firewall  string
	Clustered bool
	Yards     []ObservedYard
	Networks  map[string]Network
	// ReservationClaims includes static and DHCP IPv4 addresses across all projects
	// with their owners. Keys are network name, then occupied canonical IPv4 address.
	ReservationClaims map[string]map[string][]ReservationClaim
}

type Locker interface {
	Acquire(context.Context) (func(), error)
}

// Host performs only exact Incus operations. Policy and ownership decisions stay
// in the service, and every write uses the corresponding observation's ETag.
type Host interface {
	ReadPolicy(context.Context) (StoredPolicy, error)
	WritePolicy(context.Context, StoredPolicy, Policy) (StoredPolicy, error)
	InspectNetwork(context.Context, []Yard) (Snapshot, error)
	WriteNetworkACL(context.Context, ACL) error
	DeleteNetworkACL(context.Context, ACL) error
	WriteProfileNIC(context.Context, ObservedYard, map[string]string) error
	WriteProjectNetworks(context.Context, ObservedYard, string) error
	NetworkPower(context.Context, Yard, string) error
}

func ACLName(yard Yard) string {
	sum := sha256.Sum256([]byte(yard.Project + "/" + yard.Instance))
	return "subyard-yard-" + hex.EncodeToString(sum[:10])
}
