package yardnetwork

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"sort"
	"strings"
)

var ErrNotConverged = errors.New("yard network policy is not converged; run yard network reconcile")

type Change struct {
	Link      *Link
	Unlink    *Link
	Isolation *bool
}

type Update struct {
	Yard      ObservedYard
	NIC       map[string]string
	Access    string
	ACL       ACL
	RemoveACL bool
}

// Plan is a read-only observation bound to the exact state approved by the caller.
type Plan struct {
	Before      StoredPolicy
	Policy      Policy
	Updates     []Update
	Changed     bool
	Physical    bool
	Fingerprint string
	Change      Change
	Yards       []Yard
}

func buildPlan(stored StoredPolicy, snapshot Snapshot, change Change) (Plan, error) {
	content, err := Encode(stored.Policy)
	if err != nil {
		return Plan{}, err
	}
	p, err := Decode(content)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Before: stored, Change: change}
	known := map[string]bool{}
	for _, y := range snapshot.Yards {
		known[y.Name] = true
		plan.Yards = append(plan.Yards, y.Yard)
	}
	for _, link := range []*Link{change.Link, change.Unlink} {
		if link != nil && (!known[link.A] || !known[link.B]) {
			return Plan{}, errors.New("both endpoints must be registered local yards")
		}
	}
	if change.Link != nil {
		err = p.Link(change.Link.A, change.Link.B)
	}
	if err == nil && change.Unlink != nil {
		err = p.Unlink(change.Unlink.A, change.Unlink.B)
	}
	if err != nil {
		return Plan{}, err
	}
	if change.Isolation != nil {
		p.Isolation = *change.Isolation
	}
	for _, name := range p.Removing {
		p.Links = slices.DeleteFunc(p.Links, func(link Link) bool { return link.A == name || link.B == name })
	}
	if p.Isolation || len(p.Bindings) > 0 {
		if snapshot.Clustered || snapshot.Firewall != "nftables" {
			return Plan{}, errors.New("yard isolation requires a standalone Incus host using nftables")
		}
		if err = planBindings(&p, snapshot); err != nil {
			return Plan{}, err
		}
		if p.Isolation {
			// ACL peers identify sources by IPv4. Across bridges, overlapping
			// addresses and NAT would make that identity ambiguous.
			bridge := ""
			for _, b := range p.Bindings {
				if slices.Contains(p.Removing, b.Yard.Name) {
					continue
				}
				if bridge != "" && bridge != b.Yard.Network {
					return Plan{}, errors.New("yard isolation requires all yards on the same managed bridge")
				}
				bridge = b.Yard.Network
				for _, claim := range snapshot.ReservationClaims[b.Yard.Network][b.IPv4] {
					own := claim.Project == b.Yard.Project && (claim.Instance == b.Yard.Instance ||
						(claim.Instance == "" && claim.Profile == "default") ||
						(claim.Instance == "" && claim.Profile == "" && claim.MAC == b.MAC))
					if !own || (claim.MAC != "" && claim.MAC != b.MAC) {
						return Plan{}, fmt.Errorf("yard %s fixed IPv4 is reserved by another network identity", b.Yard.Name)
					}
				}
			}
		}
		bindings := map[string]Binding{}
		for _, b := range p.Bindings {
			bindings[b.Yard.Name] = b
		}
		for _, y := range snapshot.Yards {
			b, exists := bindings[y.Name]
			if !exists {
				continue
			}
			removing := slices.Contains(p.Removing, y.Name)
			if err = validateACLOwner(y); err != nil {
				return Plan{}, err
			}
			if removing && y.InstanceFound {
				return Plan{}, fmt.Errorf("yard %s teardown is incomplete", y.Name)
			}
			if (!y.ProjectFound || !y.ProfileFound) && !removing {
				return Plan{}, fmt.Errorf("yard %s is missing its project/profile; restore it or complete teardown", y.Name)
			}
			if y.ProfileFound {
				err = validateYard(y, b)
			} else {
				err = nil
			}
			if err != nil {
				return Plan{}, err
			}
			u := Update{Yard: y, NIC: maps.Clone(b.OriginalNIC), Access: b.OriginalAccess, ACL: y.ACL, RemoveACL: !p.Isolation || removing}
			if p.Isolation && !removing {
				u.NIC["name"] = "eth0"
				u.NIC["hwaddr"] = b.MAC
				u.NIC["ipv4.address"] = b.IPv4
				u.NIC["security.mac_filtering"] = "true"
				u.NIC["security.ipv4_filtering"] = "true"
				u.NIC["security.acls"] = ACLName(y.Yard)
				u.NIC["security.acls.default.ingress.action"] = "drop"
				u.NIC["security.acls.default.egress.action"] = "allow"
				u.Access = y.Network
				u.ACL = ACL{Name: ACLName(y.Yard), Exists: y.ACL.Exists, ETag: y.ACL.ETag, Config: map[string]string{"user.subyard.managed": "true", "user.subyard.yard": y.Name}, Egress: []Rule{{Action: "allow", State: "enabled"}}}
				for _, peer := range p.Peers(y.Name) {
					if other, ok := bindings[peer]; ok {
						u.ACL.Ingress = append(u.ACL.Ingress, Rule{Action: "allow", State: "enabled", Source: other.IPv4 + "/32"})
					}
				}
				prefix, _ := netip.ParsePrefix(snapshot.Networks[y.Network].Config["ipv4.address"])
				u.ACL.Ingress = append(u.ACL.Ingress, Rule{Action: "allow", State: "enabled", Source: prefix.Addr().String() + "/32", Protocol: "tcp", DestinationPort: "22"})
			}
			nicChange := y.ProfileFound && !maps.Equal(y.ProfileDevices["eth0"], u.NIC)
			if nicChange || (y.ProjectFound && y.ProjectConfig["restricted.networks.access"] != u.Access) || (!u.RemoveACL && !sameACL(y.ACL, u.ACL)) || (u.RemoveACL && y.ACL.Exists) {
				plan.Physical = true
				plan.Updates = append(plan.Updates, u)
			}
		}
	}
	plan.Policy = p
	encoded, err := Encode(p)
	if err != nil {
		return Plan{}, err
	}
	plan.Changed = plan.Physical || string(content) != string(encoded) || p.AppliedRevision != p.Revision || p.AppliedIsolation != p.Isolation || len(p.PendingStart) > 0 || len(p.Removing) > 0
	if plan.Changed {
		plan.Policy.Revision++
		if plan.Policy.Revision == 0 {
			return Plan{}, errors.New("network policy revision exhausted")
		}
	}
	// Runtime status, ETags, profile devices and address claims all participate.
	fingerprint, err := json.Marshal(struct {
		Stored   StoredPolicy
		Snapshot Snapshot
		Change   Change
	}{stored, snapshot, change})
	if err != nil {
		return Plan{}, err
	}
	sum := sha256.Sum256(fingerprint)
	plan.Fingerprint = hex.EncodeToString(sum[:])
	return plan, nil
}

func sameACL(a, b ACL) bool {
	return a.Exists && maps.Equal(a.Config, b.Config) && reflect.DeepEqual(a.Ingress, b.Ingress) && reflect.DeepEqual(a.Egress, b.Egress)
}

func planBindings(p *Policy, s Snapshot) error {
	bindings := map[string]Binding{}
	reserved := map[string]map[string]bool{}
	for network, claims := range s.ReservationClaims {
		reserved[network] = map[string]bool{}
		for ip := range claims {
			reserved[network][ip] = true
		}
	}
	for _, b := range p.Bindings {
		bindings[b.Yard.Name] = b
		if reserved[b.Yard.Network] == nil {
			reserved[b.Yard.Network] = map[string]bool{}
		}
		reserved[b.Yard.Network][b.IPv4] = true
	}
	for _, y := range s.Yards {
		if p.Isolation && !slices.Contains(p.Removing, y.Name) && y.ProjectConfig["features.networks"] == "true" {
			return fmt.Errorf("yard %s must use the default project's managed bridge namespace", y.Name)
		}
		if b, ok := bindings[y.Name]; ok {
			if b.Yard != y.Yard {
				return fmt.Errorf("yard %s identity changed; reconcile its registration before changing isolation", y.Name)
			}
			if p.Isolation && !slices.Contains(p.Removing, y.Name) {
				prefix, err := bridgePrefix(s.Networks[y.Network])
				if err != nil {
					return fmt.Errorf("yard %s: %w", y.Name, err)
				}
				ip, _ := netip.ParseAddr(b.IPv4)
				if !prefix.Contains(ip) || ip == prefix.Addr() || ip == prefix.Masked().Addr() || !prefix.Contains(ip.Next()) {
					return fmt.Errorf("yard %s fixed IPv4 no longer belongs to its bridge", y.Name)
				}
			}
			continue
		}
		if !p.Isolation || !y.ProjectFound {
			continue
		}
		prefix, err := bridgePrefix(s.Networks[y.Network])
		if err != nil {
			return fmt.Errorf("yard %s needs a managed IPv4 bridge with DHCP and usable addresses", y.Name)
		}
		if !y.ProfileFound {
			return fmt.Errorf("yard %s default profile is missing", y.Name)
		}
		nic := y.ProfileDevices["eth0"]
		if nic["security.acls"] != "" {
			return fmt.Errorf("yard %s has foreign NIC ACLs", y.Name)
		}
		if reserved[y.Network] == nil {
			reserved[y.Network] = map[string]bool{}
		}
		ip := nic["ipv4.address"]
		if ip == "" {
			ip = y.IPv4
		}
		if ip == "" {
			// Bound search even on a very large bridge. Existing reservations win.
			candidate := prefix.Masked().Addr().Next()
			for i := 0; i < 65536 && prefix.Contains(candidate); i++ {
				if candidate != prefix.Addr() && !reserved[y.Network][candidate.String()] && prefix.Contains(candidate.Next()) {
					ip = candidate.String()
					break
				}
				candidate = candidate.Next()
			}
		}
		address, err := netip.ParseAddr(ip)
		if err != nil || !address.Is4() || !prefix.Contains(address) || address == prefix.Addr() || address == prefix.Masked().Addr() || !prefix.Contains(address.Next()) {
			return fmt.Errorf("yard %s has no usable fixed IPv4 address", y.Name)
		}
		mac := nic["hwaddr"]
		if mac == "" {
			mac = y.MAC
		}
		if mac == "" {
			sum := sha256.Sum256([]byte(y.Project + "/" + y.Instance))
			mac = fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
		}
		parsed, err := net.ParseMAC(mac)
		if err != nil || len(parsed) != 6 || parsed[0]&1 != 0 {
			return fmt.Errorf("yard %s has an invalid NIC MAC address", y.Name)
		}
		b := Binding{Yard: y.Yard, IPv4: ip, MAC: strings.ToLower(mac), OriginalNIC: maps.Clone(nic), OriginalAccess: y.ProjectConfig["restricted.networks.access"]}
		if err = validateYard(y, b); err != nil {
			return err
		}
		for _, other := range p.Bindings {
			if other.Yard.Network == y.Network && (other.IPv4 == b.IPv4 || other.MAC == b.MAC) {
				return fmt.Errorf("yard %s network identity collides with %s", y.Name, other.Yard.Name)
			}
		}
		p.Bindings = append(p.Bindings, b)
		reserved[y.Network][ip] = true
	}
	sort.Slice(p.Bindings, func(i, j int) bool { return p.Bindings[i].Yard.Name < p.Bindings[j].Yard.Name })
	return nil
}

func validateYard(y ObservedYard, b Binding) error {
	if y.ProjectConfig["restricted"] != "true" {
		return fmt.Errorf("yard %s must use a restricted project", y.Name)
	}
	nic := y.ProfileDevices["eth0"]
	if nic["type"] != "nic" || nic["network"] != y.Network || (nic["name"] != "" && nic["name"] != "eth0") {
		return fmt.Errorf("yard %s has an unsupported primary NIC", y.Name)
	}
	for name, device := range y.ProfileDevices {
		if device["type"] == "nic" && name != "eth0" {
			return fmt.Errorf("yard %s has an extra profile NIC", y.Name)
		}
	}
	for name, device := range y.InstanceInfo.LocalDevices {
		if name == "eth0" || device["type"] == "nic" {
			return fmt.Errorf("yard %s has a local NIC override", y.Name)
		}
	}
	for name, device := range y.InstanceInfo.Devices {
		if device["type"] == "nic" && name != "eth0" {
			return fmt.Errorf("yard %s has an extra expanded NIC", y.Name)
		}
	}
	if err := validateACLOwner(y); err != nil {
		return err
	}
	if acl := nic["security.acls"]; acl != "" && acl != ACLName(y.Yard) {
		return fmt.Errorf("yard %s has foreign NIC ACLs", y.Name)
	}
	for _, user := range y.ProfileUsedBy {
		if !strings.Contains(user, "/instances/"+y.Instance+"?") && !strings.HasSuffix(user, "/instances/"+y.Instance) {
			return fmt.Errorf("yard %s profile is shared with another instance", y.Name)
		}
	}
	// Preserve unrelated fields but refuse edits to the fields this policy owns.
	owned := []string{"name", "hwaddr", "ipv4.address", "security.mac_filtering", "security.ipv4_filtering", "security.acls", "security.acls.default.ingress.action", "security.acls.default.egress.action"}
	for key, value := range nic {
		if !slices.Contains(owned, key) && b.OriginalNIC[key] != value {
			return fmt.Errorf("yard %s profile changed outside its recorded policy; restore it before reconciling", y.Name)
		}
	}
	for key, value := range b.OriginalNIC {
		if !slices.Contains(owned, key) && nic[key] != value {
			return fmt.Errorf("yard %s profile changed outside its recorded policy; restore it before reconciling", y.Name)
		}
	}
	return nil
}

func bridgePrefix(n Network) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(n.Config["ipv4.address"])
	if !n.Managed || n.Type != "bridge" || err != nil || !prefix.Addr().Is4() || prefix.Bits() > 29 || n.Config["ipv4.dhcp"] == "false" {
		return netip.Prefix{}, errors.New("isolation needs a managed IPv4 bridge with DHCP and usable addresses")
	}
	return prefix, nil
}

func validateACLOwner(y ObservedYard) error {
	if y.ACL.Exists && (y.ACL.Config["user.subyard.managed"] != "true" || y.ACL.Config["user.subyard.yard"] != y.Name) {
		return fmt.Errorf("yard %s ACL name is owned by another policy", y.Name)
	}
	return nil
}
