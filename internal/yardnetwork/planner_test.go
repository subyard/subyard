package yardnetwork

import (
	"strings"
	"testing"
)

func networkFixture() Snapshot {
	s := Snapshot{Firewall: "nftables", Networks: map[string]Network{"incusbr0": {Name: "incusbr0", Type: "bridge", Managed: true, Config: map[string]string{"ipv4.address": "10.80.0.1/24", "ipv4.dhcp": "true"}}}}
	for i, name := range []string{"a", "b", "c"} {
		y := Yard{Name: name, Project: "subyard-" + name, Instance: "yard-" + name, Network: "incusbr0"}
		s.Yards = append(s.Yards, ObservedYard{Yard: y, ProjectFound: true, ProfileFound: true, ProjectConfig: map[string]string{"restricted": "true"}, ProfileDevices: map[string]map[string]string{"eth0": {"type": "nic", "network": "incusbr0", "name": "eth0"}}, IPv4: []string{"10.80.0.10", "10.80.0.11", "10.80.0.12"}[i], MAC: []string{"00:16:3e:00:00:10", "00:16:3e:00:00:11", "00:16:3e:00:00:12"}[i], ACL: ACL{Name: ACLName(y)}})
	}
	return s
}

func TestPlannerAcceptsProductDefaultNICWithoutExplicitInterfaceName(t *testing.T) {
	s := networkFixture()
	for i := range s.Yards {
		delete(s.Yards[i].ProfileDevices["eth0"], "name")
	}
	p, _ := Decode(nil)
	on := true
	plan, err := buildPlan(StoredPolicy{Policy: p}, s, Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range plan.Updates {
		if u.NIC["name"] != "eth0" {
			t.Fatal("isolated NIC did not pin interface name")
		}
	}
	for _, b := range plan.Policy.Bindings {
		if _, ok := b.OriginalNIC["name"]; ok {
			t.Fatal("original implicit interface name lost")
		}
	}
}

func TestPlannerUsesOnlyExplicitPeersAndRestoresOriginalNIC(t *testing.T) {
	p, _ := Decode(nil)
	_ = p.Link("a", "b")
	_ = p.Link("b", "c")
	on := true
	plan, err := buildPlan(StoredPolicy{Policy: p}, networkFixture(), Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Policy.Bindings) != 3 {
		t.Fatalf("bindings=%d", len(plan.Policy.Bindings))
	}
	for _, u := range plan.Updates {
		if u.Yard.Name != "a" {
			continue
		}
		if len(u.ACL.Ingress) != 2 || u.ACL.Ingress[0].Source != "10.80.0.11/32" {
			t.Fatalf("ingress=%+v", u.ACL.Ingress)
		}
		if u.NIC["security.acls.default.ingress.action"] != "drop" || u.NIC["security.ipv4_filtering"] != "true" {
			t.Fatalf("NIC=%v", u.NIC)
		}
	}
	fixture := networkFixture()
	for i, u := range plan.Updates {
		fixture.Yards[i].ProfileDevices["eth0"] = u.NIC
		fixture.Yards[i].ACL = u.ACL
	}
	off := false
	restored, err := buildPlan(StoredPolicy{Policy: plan.Policy}, fixture, Change{Isolation: &off})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range restored.Updates {
		if u.NIC["security.acls"] != "" || u.NIC["ipv4.address"] != "" {
			t.Fatalf("not restored: %v", u.NIC)
		}
	}
	if len(restored.Policy.Links) != 2 {
		t.Fatal("mode toggle lost links")
	}
}

func TestPlannerRejectsBypassAndUnsupportedHost(t *testing.T) {
	on := true
	for _, kind := range []string{"iptables", "cluster", "extra-nic", "local-nic", "foreign-acl", "project-network-namespace"} {
		t.Run(kind, func(t *testing.T) {
			s := networkFixture()
			switch kind {
			case "iptables":
				s.Firewall = "xtables"
			case "cluster":
				s.Clustered = true
			case "extra-nic":
				s.Yards[0].ProfileDevices["eth1"] = map[string]string{"type": "nic", "network": "incusbr0"}
			case "local-nic":
				s.Yards[0].InstanceFound = true
				s.Yards[0].InstanceInfo.LocalDevices = map[string]map[string]string{"eth0": {"type": "nic"}}
			case "foreign-acl":
				s.Yards[0].ProfileDevices["eth0"]["security.acls"] = "operator-acl"
			case "project-network-namespace":
				s.Yards[0].ProjectConfig["features.networks"] = "true"
			}
			p, _ := Decode(nil)
			if _, err := buildPlan(StoredPolicy{Policy: p}, s, Change{Isolation: &on}); err == nil {
				t.Fatal("unsafe policy accepted")
			}
		})
	}
}

func TestPlannerRejectsRemovedForeignNICField(t *testing.T) {
	fixture := networkFixture()
	fixture.Yards[0].ProfileDevices["eth0"]["mtu"] = "1400"
	p, _ := Decode(nil)
	on := true
	plan, err := buildPlan(StoredPolicy{Policy: p}, fixture, Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	for index, update := range plan.Updates {
		fixture.Yards[index].ProfileDevices["eth0"] = update.NIC
		fixture.Yards[index].ACL = update.ACL
	}
	delete(fixture.Yards[0].ProfileDevices["eth0"], "mtu")
	if _, err := buildPlan(StoredPolicy{Policy: plan.Policy}, fixture, Change{}); err == nil {
		t.Fatal("removal of a recorded foreign NIC field was silently overwritten")
	}
}

func TestPlannerRejectsChangedBridgeForExistingBinding(t *testing.T) {
	p, _ := Decode(nil)
	on := true
	plan, err := buildPlan(StoredPolicy{Policy: p}, networkFixture(), Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	fixture := networkFixture()
	fixture.Networks["incusbr0"] = Network{
		Name: "incusbr0", Type: "bridge", Managed: true,
		Config: map[string]string{"ipv4.address": "10.81.0.1/24", "ipv4.dhcp": "true"},
	}
	if _, err := buildPlan(StoredPolicy{Policy: plan.Policy}, fixture, Change{}); err == nil {
		t.Fatal("existing fixed identities outside the changed bridge were accepted")
	}
}

func TestPlannerRejectsIsolationAcrossBridgesWithOverlappingAddresses(t *testing.T) {
	for _, linked := range []bool{false, true} {
		s := networkFixture()
		s.Networks["otherbr0"] = Network{Name: "otherbr0", Type: "bridge", Managed: true, Config: map[string]string{"ipv4.address": "10.80.0.1/24", "ipv4.dhcp": "true"}}
		s.Yards[1].Network = "otherbr0"
		s.Yards[1].ProfileDevices["eth0"]["network"] = "otherbr0"
		s.Yards[1].IPv4 = s.Yards[2].IPv4
		p, _ := Decode(nil)
		if linked {
			_ = p.Link("a", "b")
		}
		if _, err := buildPlan(StoredPolicy{Policy: p}, s, Change{}); err != nil {
			t.Fatalf("saving links with isolation off: %v", err)
		}
		on := true
		if _, err := buildPlan(StoredPolicy{Policy: p}, s, Change{Isolation: &on}); err == nil || !strings.Contains(err.Error(), "same managed bridge") {
			t.Fatalf("linked=%v: overlapping cross-bridge source identities accepted: %v", linked, err)
		}
	}
}

func TestPlannerAllowsDisablingLegacyCrossBridgePolicy(t *testing.T) {
	s := networkFixture()
	p, _ := Decode(nil)
	on := true
	plan, err := buildPlan(StoredPolicy{Policy: p}, s, Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	for i, u := range plan.Updates {
		s.Yards[i].ProfileDevices["eth0"] = u.NIC
		s.Yards[i].ACL = u.ACL
	}
	s.Yards[1].Network = "otherbr0"
	s.Yards[1].ProfileDevices["eth0"]["network"] = "otherbr0"
	plan.Policy.Bindings[1].Yard.Network = "otherbr0"
	plan.Policy.Bindings[1].OriginalNIC["network"] = "otherbr0"
	off := false
	restored, err := buildPlan(StoredPolicy{Policy: plan.Policy}, s, Change{Isolation: &off})
	if err != nil {
		t.Fatalf("unsupported topology prevented rollback: %v", err)
	}
	for _, u := range restored.Updates {
		if !u.RemoveACL || u.NIC["security.acls"] != "" {
			t.Fatal("rollback retained isolation")
		}
	}
}

func TestPlannerRejectsIPv4ClaimedByAnotherNetworkIdentity(t *testing.T) {
	for _, claim := range []ReservationClaim{
		{Project: "foreign", Instance: "other", MAC: "00:16:3e:00:00:99"},
		{Project: "subyard-a", Instance: "other", MAC: "00:16:3e:00:00:10"},
		{Project: "subyard-a", Instance: "yard-a", MAC: "00:16:3e:00:00:99"},
		{Project: "subyard-a", Profile: "other"},
		{Project: "foreign", MAC: "00:16:3e:00:00:10"},
		{Project: "subyard-a", MAC: "00:16:3e:00:00:99"},
	} {
		s := networkFixture()
		s.ReservationClaims = map[string]map[string][]ReservationClaim{
			"incusbr0": {s.Yards[0].IPv4: {claim}},
		}
		p, _ := Decode(nil)
		on := true
		if _, err := buildPlan(StoredPolicy{Policy: p}, s, Change{Isolation: &on}); err == nil || !strings.Contains(err.Error(), "reserved by another network identity") {
			t.Fatalf("claim %+v did not reject ambiguous source identity: %v", claim, err)
		}
	}
}

func TestPlannerAcceptsItsOwnProfileInstanceAndLeaseClaims(t *testing.T) {
	s := networkFixture()
	s.ReservationClaims = map[string]map[string][]ReservationClaim{
		"incusbr0": {s.Yards[0].IPv4: {
			{Project: "subyard-a", Instance: "yard-a", MAC: s.Yards[0].MAC},
			{Project: "subyard-a", Profile: "default"},
			{Project: "subyard-a", MAC: s.Yards[0].MAC},
		}},
	}
	p, _ := Decode(nil)
	on := true
	plan, err := buildPlan(StoredPolicy{Policy: p}, s, Change{Isolation: &on})
	if err != nil {
		t.Fatalf("own IP claims rejected: %v", err)
	}
	// A later foreign claim must block reconciliation of an existing binding,
	// while disabling isolation remains possible to recover original settings.
	s.ReservationClaims["incusbr0"][s.Yards[0].IPv4] = append(s.ReservationClaims["incusbr0"][s.Yards[0].IPv4], ReservationClaim{Project: "foreign", Instance: "other"})
	if _, err := buildPlan(StoredPolicy{Policy: plan.Policy}, s, Change{}); err == nil {
		t.Fatal("existing binding accepted a new foreign IP claim")
	}
	off := false
	if _, err := buildPlan(StoredPolicy{Policy: plan.Policy}, s, Change{Isolation: &off}); err != nil {
		t.Fatalf("conflicting reservation blocked isolation rollback: %v", err)
	}
}

func TestPlannerAllocatesAroundForeignAddressClaims(t *testing.T) {
	s := networkFixture()
	s.Yards[0].IPv4 = ""
	s.ReservationClaims = map[string]map[string][]ReservationClaim{
		"incusbr0": {"10.80.0.2": {{Project: "foreign", Instance: "other"}}},
	}
	p, _ := Decode(nil)
	on := true
	plan, err := buildPlan(StoredPolicy{Policy: p}, s, Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Policy.Bindings[0].IPv4; got != "10.80.0.3" {
		t.Fatalf("allocated %s, want first unclaimed address 10.80.0.3", got)
	}
}
