package incusclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Subyard/Subyard/internal/yardnetwork"
	"github.com/lxc/incus/v6/shared/api"
)

func TestNetworkPolicyUsesDefaultProjectETagAndPreservesForeignConfig(t *testing.T) {
	fake := newNetworkIncusServer(t)
	fake.projects["default"] = fakeProject{
		ETag:        "project-v1",
		Description: "Default project",
		Config: map[string]string{
			"features.profiles":   "true",
			yardnetwork.PolicyKey: `{"schema":1,"isolation":false,"revision":1,"appliedRevision":1}`,
		},
	}
	client := New(fake.socket, "projects")

	stored, err := client.ReadPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stored.ETag != "project-v1" || stored.Policy.Revision != 1 || stored.Policy.Isolation {
		t.Fatalf("unexpected stored policy: %+v", stored)
	}
	desired := yardnetwork.Policy{
		Schema: yardnetwork.SchemaVersion, Isolation: true, Revision: 2, AppliedRevision: 1,
		Links: []yardnetwork.Link{{A: "alpha", B: "beta"}},
	}
	written, err := client.WritePolicy(context.Background(), stored, desired)
	if err != nil {
		t.Fatal(err)
	}
	wantContent := `{"schema":1,"isolation":true,"links":[{"a":"alpha","b":"beta"}],"revision":2,"appliedRevision":1,"appliedIsolation":false}`
	if written.ETag != "project-v2" || written.Content != wantContent || !reflect.DeepEqual(written.Policy, desired) {
		t.Fatalf("unexpected write result: %+v", written)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	project := fake.projects["default"]
	if fake.projectIfMatch != "project-v1" {
		t.Fatalf("project If-Match = %q, want observed ETag", fake.projectIfMatch)
	}
	if project.Description != "Default project" || project.Config["features.profiles"] != "true" ||
		project.Config[yardnetwork.PolicyKey] != wantContent {
		t.Fatalf("project update lost foreign fields: %+v", project)
	}
}

func TestNetworkPolicyRejectsMalformedStoredJSON(t *testing.T) {
	fake := newNetworkIncusServer(t)
	fake.projects["default"] = fakeProject{
		ETag: "project-v1",
		Config: map[string]string{
			yardnetwork.PolicyKey: `{"schema":1,"foreign":true}`,
		},
	}
	if _, err := New(fake.socket, "projects").ReadPolicy(context.Background()); err == nil {
		t.Fatal("malformed stored policy was accepted")
	}
}

func TestInspectNetworkReadsLiveYardAndCrossProjectReservationsWithoutMutation(t *testing.T) {
	fake := newNetworkIncusServer(t)
	fake.clustered = true
	fake.networks["incusbr0"] = fakeNetwork{
		Type: "bridge", Managed: true,
		Config: map[string]string{"ipv4.address": "10.44.0.1/24", "ipv4.nat": "true"},
	}
	fake.networks["host0"] = fakeNetwork{Type: "physical", Managed: false}
	fake.leases["project-a/incusbr0"] = []map[string]any{
		{"hostname": "yard-a", "hwaddr": "00:16:3e:aa:bb:cc", "address": "10.44.0.10", "type": "dynamic"},
	}
	fake.leases["default/incusbr0"] = []map[string]any{
		{"hostname": "other", "hwaddr": "00:16:3e:00:00:20", "address": "10.44.0.20", "type": "dynamic"},
		{"hostname": "v6", "address": "fd42::2", "type": "dynamic"},
	}
	fake.leases["foreign/incusbr0"] = []map[string]any{
		{"hostname": "untrusted-hostname", "hwaddr": "00:16:3e:00:00:52", "address": "10.44.0.50", "type": "dynamic"},
		{"hostname": "also-untrusted", "hwaddr": "00:16:3e:00:00:51", "address": "10.44.0.10", "type": "dynamic"},
	}
	fake.projects["default"] = fakeProject{ETag: "default-v1", Config: map[string]string{"features.profiles": "true"}}
	fake.projects["project-a"] = fakeProject{
		ETag: "project-a-v1", Description: "yard project",
		Config: map[string]string{"restricted": "true", "foreign": "kept"},
	}
	fake.projects["foreign"] = fakeProject{ETag: "foreign-v1", Config: map[string]string{"features.profiles": "true"}}
	fake.projects["own-network"] = fakeProject{ETag: "own-network-v1", Config: map[string]string{"features.networks": "true"}}
	fake.profiles["project-a/default"] = fakeProfile{
		ETag: "profile-a-v1", Description: "yard defaults", UsedBy: []string{"/1.0/instances/yard-a?project=project-a"},
		Config: map[string]string{"limits.cpu": "4"},
		Devices: map[string]map[string]string{
			"root": {"type": "disk", "path": "/", "pool": "default"},
			"eth0": {"type": "nic", "network": "incusbr0", "name": "eth0", "ipv4.address": "10.44.0.10", "hwaddr": "00:16:3e:aa:bb:cc"},
		},
	}
	fake.profiles["foreign/default"] = fakeProfile{
		ETag: "foreign-profile-v1",
		Devices: map[string]map[string]string{
			"eth0": {"type": "nic", "parent": "incusbr0", "ipv4.address": "10.44.0.30"},
		},
	}
	fake.profiles["own-network/default"] = fakeProfile{Devices: map[string]map[string]string{
		"eth0": {"type": "nic", "network": "incusbr0", "ipv4.address": "10.44.0.60"},
	}}
	fake.instances["project-a/yard-a"] = fakeInstance{
		ETag: "instance-a-v1",
		Data: map[string]any{
			"name": "yard-a", "project": "project-a", "type": "container", "status": "Running",
			"config":          map[string]string{"volatile.eth0.hwaddr": "00:16:3e:aa:bb:cc"},
			"devices":         map[string]map[string]string{},
			"expanded_config": map[string]string{"volatile.eth0.hwaddr": "00:16:3e:aa:bb:cc", "security.nesting": "true"},
			"expanded_devices": map[string]map[string]string{
				"eth0": {"type": "nic", "network": "incusbr0", "name": "eth0"},
			},
		},
		State: map[string]any{"status": "Running", "network": map[string]any{
			"eth0": map[string]any{
				"hwaddr":    "00:16:3e:aa:bb:cc",
				"addresses": []map[string]any{{"family": "inet", "address": "10.44.0.10", "scope": "global"}},
			},
		}},
	}
	fake.instances["foreign/tool"] = fakeInstance{
		ETag: "foreign-instance-v1",
		Data: map[string]any{
			"name": "tool", "project": "foreign", "type": "container", "status": "Stopped",
			"devices": map[string]map[string]string{
				"lan": {"type": "nic", "network": "incusbr0", "ipv4.address": "10.44.0.40"},
			},
		},
	}
	fake.instances["foreign/dynamic"] = fakeInstance{Data: map[string]any{
		"name": "dynamic", "project": "foreign", "type": "container", "status": "Running",
		"config":          map[string]string{"volatile.eth0.hwaddr": "00:16:3e:00:00:52"},
		"expanded_config": map[string]string{"volatile.eth0.hwaddr": "00:16:3e:00:00:52"},
		"devices":         map[string]map[string]string{},
		"expanded_devices": map[string]map[string]string{
			"eth0": {"type": "nic", "network": "incusbr0", "name": "eth0"},
		},
	}}
	fake.instances["foreign/collider"] = fakeInstance{Data: map[string]any{
		"name": "collider", "project": "foreign", "type": "container", "status": "Running",
		"config":          map[string]string{"volatile.eth0.hwaddr": "00:16:3e:00:00:51"},
		"expanded_config": map[string]string{"volatile.eth0.hwaddr": "00:16:3e:00:00:51"},
		"devices":         map[string]map[string]string{},
		"expanded_devices": map[string]map[string]string{
			"eth0": {"type": "nic", "network": "incusbr0", "name": "eth0"},
		},
	}}
	fake.instances["own-network/local"] = fakeInstance{Data: map[string]any{
		"name": "local", "project": "own-network", "type": "container", "status": "Stopped",
		"devices": map[string]map[string]string{
			"eth0": {"type": "nic", "network": "incusbr0", "ipv4.address": "10.44.0.61"},
		},
	}}
	fake.instances["foreign/yard-zeta"] = fakeInstance{
		ETag: "managed-instance-v1",
		Data: map[string]any{
			"name": "yard-zeta", "project": "foreign", "type": "container", "status": "Stopped",
			"config": map[string]string{
				"user.subyard.managed": "true", "user.subyard.name": "zeta", "user.subyard.bridge": "incusbr0",
				"volatile.eth0.hwaddr": "00:16:3e:00:00:50",
			},
			"expanded_config": map[string]string{
				"user.subyard.managed": "true", "user.subyard.name": "zeta", "user.subyard.bridge": "incusbr0",
				"volatile.eth0.hwaddr": "00:16:3e:00:00:50",
			},
			"devices": map[string]map[string]string{},
			"expanded_devices": map[string]map[string]string{
				"eth0": {"type": "nic", "network": "incusbr0", "name": "eth0"},
			},
		},
		State: map[string]any{"status": "Stopped", "network": map[string]any{}},
	}
	yard := yardnetwork.Yard{Name: "alpha", Project: "project-a", Instance: "yard-a", Network: "incusbr0"}
	aclName := yardnetwork.ACLName(yard)
	fake.acls[aclName] = fakeACL{
		ETag: "acl-a-v1", Config: map[string]string{"user.subyard.owner": "project-a/yard-a"},
		Ingress: []map[string]any{{"action": "allow", "state": "enabled", "source": "10.44.0.20/32", "protocol": "tcp", "destination_port": "22"}},
		Egress:  []map[string]any{{"action": "reject", "state": "enabled", "destination": "10.44.0.0/24"}},
	}

	snapshot, err := New(fake.socket, "projects").InspectNetwork(context.Background(), []yardnetwork.Yard{
		yard,
		{Name: "empty", Project: "default", Instance: "absent-instance", Network: "incusbr0"},
		{Name: "missing", Project: "missing-project", Instance: "missing-instance", Network: "incusbr0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Firewall != "nftables" || !snapshot.Clustered {
		t.Fatalf("server environment was not projected: %+v", snapshot)
	}
	if len(snapshot.Networks) != 1 || snapshot.Networks["incusbr0"].Config["ipv4.address"] != "10.44.0.1/24" {
		t.Fatalf("managed default networks = %+v", snapshot.Networks)
	}
	wantReservations := []string{"10.44.0.10", "10.44.0.20", "10.44.0.30", "10.44.0.40", "10.44.0.50"}
	reservations := make([]string, 0, len(snapshot.ReservationClaims["incusbr0"]))
	for address := range snapshot.ReservationClaims["incusbr0"] {
		reservations = append(reservations, address)
	}
	sort.Strings(reservations)
	if !reflect.DeepEqual(reservations, wantReservations) {
		t.Fatalf("reservations = %v, want %v", reservations, wantReservations)
	}
	if claims := snapshot.ReservationClaims["incusbr0"]["10.44.0.10"]; !reflect.DeepEqual(claims,
		[]yardnetwork.ReservationClaim{
			{Project: "foreign", Instance: "collider", MAC: "00:16:3e:00:00:51"},
			{Project: "project-a", Instance: "yard-a", MAC: "00:16:3e:aa:bb:cc"},
		}) {
		t.Fatalf("same-owner observations merged or distinct owner collision lost: %+v", claims)
	}
	if claims := snapshot.ReservationClaims["incusbr0"]["10.44.0.50"]; !reflect.DeepEqual(claims,
		[]yardnetwork.ReservationClaim{{Project: "foreign", Instance: "dynamic", MAC: "00:16:3e:00:00:52"}}) {
		t.Fatalf("foreign dynamic lease owner was not resolved by MAC: %+v", claims)
	}
	if len(snapshot.Yards) != 4 {
		t.Fatalf("yards = %+v", snapshot.Yards)
	}
	observed := snapshot.Yards[0]
	if !observed.ProjectFound || observed.ProjectETag != "project-a-v1" || observed.ProjectConfig["foreign"] != "kept" ||
		!observed.ProfileFound || observed.ProfileETag != "profile-a-v1" ||
		!reflect.DeepEqual(observed.ProfileUsedBy, []string{"/1.0/instances/yard-a?project=project-a"}) ||
		!observed.InstanceFound || observed.InstanceInfo.Name != "yard-a" || observed.InstanceInfo.Status != "Running" ||
		observed.IPv4 != "10.44.0.10" || observed.MAC != "00:16:3e:aa:bb:cc" {
		t.Fatalf("yard observation incomplete: %+v", observed)
	}
	if !observed.ACL.Exists || observed.ACL.Name != aclName || observed.ACL.ETag != "acl-a-v1" ||
		len(observed.ACL.Ingress) != 1 || observed.ACL.Ingress[0].DestinationPort != "22" ||
		len(observed.ACL.Egress) != 1 || observed.ACL.Egress[0].Destination != "10.44.0.0/24" {
		t.Fatalf("owned ACL observation incomplete: %+v", observed.ACL)
	}
	empty := snapshot.Yards[1]
	if !empty.ProjectFound || empty.ProfileFound || empty.InstanceFound || empty.ACL.Exists {
		t.Fatalf("absent profile, instance, or ACL was fabricated: %+v", empty)
	}
	missing := snapshot.Yards[2]
	if missing.ProjectFound || missing.ProfileFound || missing.InstanceFound || missing.ACL.Exists {
		t.Fatalf("absent resources were fabricated: %+v", missing)
	}
	discovered := snapshot.Yards[3]
	if discovered.Name != "zeta" || discovered.Project != "foreign" || discovered.Instance != "yard-zeta" ||
		discovered.Network != "incusbr0" || !discovered.ProjectFound || !discovered.InstanceFound {
		t.Fatalf("managed yard outside requested registrations was omitted: %+v", discovered)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, method := range fake.methods {
		if method != http.MethodGet {
			t.Fatalf("inspection mutated Incus through %s", method)
		}
	}
}

func TestInspectNetworkRejectsMalformedOrConflictingManagedInventory(t *testing.T) {
	for _, test := range []struct {
		name      string
		config    map[string]string
		requested []yardnetwork.Yard
	}{
		{
			name: "malformed marker",
			config: map[string]string{
				"user.subyard.managed": "yes", "user.subyard.name": "zeta", "user.subyard.bridge": "incusbr0",
			},
		},
		{
			name: "missing bridge",
			config: map[string]string{
				"user.subyard.managed": "true", "user.subyard.name": "zeta",
			},
		},
		{
			name: "conflicting yard name",
			config: map[string]string{
				"user.subyard.managed": "true", "user.subyard.name": "alpha", "user.subyard.bridge": "incusbr0",
			},
			requested: []yardnetwork.Yard{{Name: "alpha", Project: "project-a", Instance: "yard-a", Network: "incusbr0"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			fake.projects["default"] = fakeProject{ETag: "default-v1"}
			fake.instances["default/managed"] = fakeInstance{Data: map[string]any{
				"name": "managed", "project": "default", "type": "container", "status": "Stopped",
				"config": test.config, "expanded_config": test.config,
			}}
			_, err := New(fake.socket, "projects").InspectNetwork(context.Background(), test.requested)
			if err == nil || !strings.Contains(err.Error(), "managed network metadata") {
				t.Fatalf("managed inventory error = %v", err)
			}
		})
	}
}

func TestLeaseReservationClaimsRetainEveryMatchingInstance(t *testing.T) {
	const mac = "00:16:3e:00:00:70"
	instances := []api.Instance{
		{Name: "first", ExpandedDevices: map[string]map[string]string{
			"eth0": {"type": "nic", "network": "incusbr0", "hwaddr": mac},
		}},
		{Name: "second", ExpandedDevices: map[string]map[string]string{
			"eth0": {"type": "nic", "network": "incusbr0", "hwaddr": mac},
		}},
	}
	claims := leaseReservationClaims("project-a", "incusbr0", &api.NetworkLease{Hwaddr: mac}, instances)
	want := []yardnetwork.ReservationClaim{
		{Project: "project-a", Instance: "first", MAC: mac},
		{Project: "project-a", Instance: "second", MAC: mac},
	}
	if !reflect.DeepEqual(claims, want) {
		t.Fatalf("ambiguous MAC ownership collapsed: got %+v, want %+v", claims, want)
	}
}

func TestInspectNetworkReturnsEndpointFailures(t *testing.T) {
	fake := newNetworkIncusServer(t)
	fake.failures["GET /1.0/networks"] = http.StatusServiceUnavailable
	_, err := New(fake.socket, "projects").InspectNetwork(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "list default project networks") {
		t.Fatalf("network endpoint failure = %v", err)
	}
}

func TestWriteNetworkACLSelectsExactCreateOrETagUpdate(t *testing.T) {
	fake := newNetworkIncusServer(t)
	client := New(fake.socket, "projects")
	desired := yardnetwork.ACL{
		Name: "subyard-yard-owned", Config: map[string]string{"user.subyard.owner": "project-a/yard-a"},
		Ingress: []yardnetwork.Rule{{Action: "allow", State: "enabled", Source: "10.44.0.20/32", Protocol: "tcp", DestinationPort: "22"}},
		Egress:  []yardnetwork.Rule{{Action: "reject", State: "enabled", Destination: "10.44.0.0/24"}},
	}
	if err := client.WriteNetworkACL(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	created := fake.acls[desired.Name]
	fake.mu.Unlock()
	if created.Config["user.subyard.owner"] != "project-a/yard-a" || len(created.Ingress) != 1 ||
		created.Ingress[0]["destination_port"] != "22" {
		t.Fatalf("created ACL = %+v", created)
	}

	desired.Exists = true
	desired.ETag = "acl-v1"
	desired.Ingress[0].DestinationPort = "2222"
	if err := client.WriteNetworkACL(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	updated := fake.acls[desired.Name]
	if fake.aclIfMatch != "acl-v1" || len(updated.Ingress) != 1 || updated.Ingress[0]["destination_port"] != "2222" {
		t.Fatalf("ACL update did not use observation: If-Match=%q ACL=%+v", fake.aclIfMatch, updated)
	}
}

func TestDeleteNetworkACLRechecksObservedETagBeforeExactDelete(t *testing.T) {
	fake := newNetworkIncusServer(t)
	name := "subyard-yard-owned"
	fake.acls[name] = fakeACL{ETag: "acl-v1", Config: map[string]string{"user.subyard.owner": "project-a/yard-a"}}
	client := New(fake.socket, "projects")
	if err := client.DeleteNetworkACL(context.Background(), yardnetwork.ACL{Name: name, Exists: true, ETag: "stale"}); err == nil {
		t.Fatal("stale ACL observation was deleted")
	}
	fake.mu.Lock()
	_, exists := fake.acls[name]
	fake.mu.Unlock()
	if !exists {
		t.Fatal("stale ACL was removed")
	}
	if err := client.DeleteNetworkACL(context.Background(), yardnetwork.ACL{Name: name, Exists: true, ETag: "acl-v1"}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	_, exists = fake.acls[name]
	fake.mu.Unlock()
	if exists {
		t.Fatal("matching exact ACL was not removed")
	}
}

func TestWriteProfileNICAndProjectNetworksPreserveForeignFieldsWithObservedETags(t *testing.T) {
	fake := newNetworkIncusServer(t)
	fake.projects["project-a"] = fakeProject{
		ETag: "project-v1", Description: "keep project description",
		Config: map[string]string{"restricted": "true", "foreign.project": "keep"},
	}
	fake.profiles["project-a/default"] = fakeProfile{
		ETag: "profile-v1", Description: "keep profile description",
		Config: map[string]string{"limits.cpu": "4"},
		Devices: map[string]map[string]string{
			"root": {"type": "disk", "path": "/", "pool": "default"},
			"eth0": {"type": "nic", "network": "incusbr0", "name": "eth0", "foreign.nic": "keep"},
			"gpu":  {"type": "gpu"},
		},
	}
	observed := yardnetwork.ObservedYard{
		Yard:         yardnetwork.Yard{Project: "project-a", Instance: "yard-a", Network: "incusbr0"},
		ProjectFound: true, ProjectETag: "project-v1", ProfileFound: true, ProfileETag: "profile-v1",
	}
	client := New(fake.socket, "projects")
	if err := client.WriteProfileNIC(context.Background(), observed, map[string]string{
		"type": "nic", "network": "incusbr0", "name": "eth0", "security.acls": "subyard-yard-owned",
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteProjectNetworks(context.Background(), observed, "incusbr0"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	profile := fake.profiles["project-a/default"]
	if fake.profileIfMatch != "profile-v1" || profile.Description != "keep profile description" ||
		profile.Config["limits.cpu"] != "4" || profile.Devices["root"]["pool"] != "default" ||
		profile.Devices["gpu"]["type"] != "gpu" || profile.Devices["eth0"]["security.acls"] != "subyard-yard-owned" ||
		profile.Devices["eth0"]["foreign.nic"] != "" {
		t.Fatalf("profile update failed to replace only eth0: If-Match=%q profile=%+v", fake.profileIfMatch, profile)
	}
	project := fake.projects["project-a"]
	if fake.projectIfMatch != "project-v1" || project.Description != "keep project description" ||
		project.Config["restricted"] != "true" || project.Config["foreign.project"] != "keep" ||
		project.Config["restricted.networks.access"] != "incusbr0" {
		t.Fatalf("project update lost foreign fields: If-Match=%q project=%+v", fake.projectIfMatch, project)
	}
}

func TestNetworkPowerUsesBoundedGracefulOfficialOperation(t *testing.T) {
	fake := newNetworkIncusServer(t)
	fake.instances["project-a/yard-a"] = fakeInstance{Data: map[string]any{
		"name": "yard-a", "project": "project-a", "type": "container", "status": "Running",
	}}
	client := New(fake.socket, "projects")
	yard := yardnetwork.Yard{Project: "project-a", Instance: "yard-a"}
	for _, action := range []string{"stop", "start"} {
		if err := client.NetworkPower(context.Background(), yard, action); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.NetworkPower(context.Background(), yard, "restart"); err == nil {
		t.Fatal("unsupported power action was accepted")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.powerCalls) != 2 || fake.powerCalls[0].Action != "stop" || fake.powerCalls[0].Timeout != 30 || fake.powerCalls[0].Force ||
		fake.powerCalls[1].Action != "start" || fake.powerCalls[1].Timeout != 30 || fake.powerCalls[1].Force ||
		len(fake.operationWaitBounded) != 2 || !fake.operationWaitBounded[0] || !fake.operationWaitBounded[1] {
		t.Fatalf("power calls were not bounded and graceful: %+v", fake.powerCalls)
	}
}

type fakeProject struct {
	ETag        string
	Description string
	Config      map[string]string
}

type fakeNetwork struct {
	Type    string
	Managed bool
	Config  map[string]string
}

type fakeProfile struct {
	ETag        string
	Description string
	Config      map[string]string
	Devices     map[string]map[string]string
	UsedBy      []string
}

type fakeInstance struct {
	ETag  string
	Data  map[string]any
	State map[string]any
}

type fakeACL struct {
	ETag    string
	Config  map[string]string
	Ingress []map[string]any
	Egress  []map[string]any
}

type fakePowerCall struct {
	Project string
	Name    string
	Action  string
	Timeout int
	Force   bool
}

type networkIncusServer struct {
	socket string

	mu                   sync.Mutex
	server               *http.Server
	listener             net.Listener
	projects             map[string]fakeProject
	networks             map[string]fakeNetwork
	leases               map[string][]map[string]any
	profiles             map[string]fakeProfile
	instances            map[string]fakeInstance
	acls                 map[string]fakeACL
	failures             map[string]int
	methods              []string
	clustered            bool
	projectIfMatch       string
	profileIfMatch       string
	aclIfMatch           string
	powerCalls           []fakePowerCall
	operationWaitBounded []bool
	nextOperation        int
}

func newNetworkIncusServer(t *testing.T) *networkIncusServer {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "incus.socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	fake := &networkIncusServer{
		socket: socket, listener: listener,
		projects: make(map[string]fakeProject), networks: make(map[string]fakeNetwork),
		leases: make(map[string][]map[string]any), profiles: make(map[string]fakeProfile),
		instances: make(map[string]fakeInstance), acls: make(map[string]fakeACL),
		failures: make(map[string]int),
	}
	fake.server = &http.Server{Handler: fake}
	go func() { _ = fake.server.Serve(listener) }()
	t.Cleanup(func() {
		_ = fake.server.Close()
		_ = fake.listener.Close()
		_ = os.Remove(socket)
	})
	return fake
}

func (fake *networkIncusServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	fake.mu.Lock()
	fake.methods = append(fake.methods, request.Method)
	failure := fake.failures[request.Method+" "+request.URL.Path]
	fake.mu.Unlock()
	if failure != 0 {
		writeNetworkIncusError(writer, failure, "injected failure")
		return
	}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/1.0":
		writeNetworkIncusSync(writer, map[string]any{
			"api_extensions": []string{"projects", "instances", "network", "network_leases", "network_acl"},
			"environment": map[string]any{
				"server": "incus", "server_version": "6.23", "firewall": "nftables",
				"server_clustered": fake.clustered,
			},
		})
	case request.Method == http.MethodGet && request.URL.Path == "/1.0/networks":
		fake.getNetworks(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/1.0/networks/") &&
		strings.HasSuffix(request.URL.Path, "/leases"):
		name := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/1.0/networks/"), "/leases")
		project := request.URL.Query().Get("project")
		fake.mu.Lock()
		leases := fake.leases[project+"/"+name]
		fake.mu.Unlock()
		writeNetworkIncusSync(writer, leases)
	case request.Method == http.MethodGet && request.URL.Path == "/1.0/projects":
		fake.getProjects(writer)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/1.0/projects/"):
		name, _ := url.PathUnescape(strings.TrimPrefix(request.URL.Path, "/1.0/projects/"))
		fake.mu.Lock()
		project, ok := fake.projects[name]
		fake.mu.Unlock()
		if !ok {
			writeNetworkIncusError(writer, http.StatusNotFound, "project not found")
			return
		}
		writer.Header().Set("ETag", project.ETag)
		writeNetworkIncusSync(writer, map[string]any{
			"name": name, "description": project.Description, "config": project.Config,
		})
	case request.Method == http.MethodGet && request.URL.Path == "/1.0/profiles":
		fake.getProfiles(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/1.0/profiles/"):
		fake.getProfile(writer, request)
	case request.Method == http.MethodGet && request.URL.Path == "/1.0/instances":
		fake.getInstances(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/1.0/instances/") &&
		strings.HasSuffix(request.URL.Path, "/state"):
		fake.getInstanceState(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/1.0/instances/"):
		fake.getInstance(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/1.0/network-acls/"):
		fake.getACL(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/1.0/network-acls":
		fake.createACL(writer, request)
	case request.Method == http.MethodPut && strings.HasPrefix(request.URL.Path, "/1.0/network-acls/"):
		fake.updateACL(writer, request)
	case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/1.0/network-acls/"):
		fake.deleteACL(writer, request)
	case request.Method == http.MethodPut && strings.HasPrefix(request.URL.Path, "/1.0/projects/"):
		fake.updateProject(writer, request)
	case request.Method == http.MethodPut && strings.HasPrefix(request.URL.Path, "/1.0/profiles/"):
		fake.updateProfile(writer, request)
	case request.Method == http.MethodPut && strings.HasPrefix(request.URL.Path, "/1.0/instances/") &&
		strings.HasSuffix(request.URL.Path, "/state"):
		fake.updatePower(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/1.0/operations/") &&
		strings.HasSuffix(request.URL.Path, "/wait"):
		fake.mu.Lock()
		fake.operationWaitBounded = append(fake.operationWaitBounded,
			request.URL.Query().Get("timeout") != "" && request.URL.Query().Get("timeout") != "-1")
		fake.mu.Unlock()
		writeNetworkIncusSync(writer, map[string]any{
			"id":    strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/1.0/operations/"), "/wait"),
			"class": "task", "status": "Success", "status_code": 200, "metadata": map[string]any{},
		})
	default:
		writeNetworkIncusError(writer, http.StatusNotFound,
			fmt.Sprintf("endpoint not found: %s %s?%s", request.Method, request.URL.Path, request.URL.RawQuery))
	}
}

func (fake *networkIncusServer) updateProject(writer http.ResponseWriter, request *http.Request) {
	name, _ := url.PathUnescape(strings.TrimPrefix(request.URL.Path, "/1.0/projects/"))
	var body struct {
		Description string            `json:"description"`
		Config      map[string]string `json:"config"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeNetworkIncusError(writer, http.StatusBadRequest, "bad project")
		return
	}
	fake.mu.Lock()
	fake.projectIfMatch = request.Header.Get("If-Match")
	project := fake.projects[name]
	if fake.projectIfMatch != project.ETag {
		fake.mu.Unlock()
		writeNetworkIncusError(writer, http.StatusPreconditionFailed, "stale project")
		return
	}
	project.Description = body.Description
	project.Config = body.Config
	project.ETag = strings.TrimSuffix(project.ETag, "v1") + "v2"
	fake.projects[name] = project
	fake.mu.Unlock()
	writeNetworkIncusSync(writer, map[string]any{})
}

func (fake *networkIncusServer) getNetworks(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Query().Get("project") == "" || request.URL.Query().Get("recursion") != "1" {
		writeNetworkIncusError(writer, http.StatusBadRequest, "project recursive networks required")
		return
	}
	fake.mu.Lock()
	names := make([]string, 0, len(fake.networks))
	for name := range fake.networks {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]map[string]any, 0, len(names))
	for _, name := range names {
		network := fake.networks[name]
		result = append(result, map[string]any{
			"name": name, "type": network.Type, "managed": network.Managed, "config": network.Config,
		})
	}
	fake.mu.Unlock()
	writeNetworkIncusSync(writer, result)
}

func (fake *networkIncusServer) getProjects(writer http.ResponseWriter) {
	fake.mu.Lock()
	names := make([]string, 0, len(fake.projects))
	for name := range fake.projects {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]map[string]any, 0, len(names))
	for _, name := range names {
		project := fake.projects[name]
		result = append(result, map[string]any{"name": name, "description": project.Description, "config": project.Config})
	}
	fake.mu.Unlock()
	writeNetworkIncusSync(writer, result)
}

func (fake *networkIncusServer) getProfiles(writer http.ResponseWriter, request *http.Request) {
	project := request.URL.Query().Get("project")
	fake.mu.Lock()
	result := make([]map[string]any, 0)
	for key, profile := range fake.profiles {
		candidateProject, name, _ := strings.Cut(key, "/")
		if candidateProject == project {
			result = append(result, profileMetadata(project, name, profile))
		}
	}
	fake.mu.Unlock()
	writeNetworkIncusSync(writer, result)
}

func (fake *networkIncusServer) getProfile(writer http.ResponseWriter, request *http.Request) {
	project := request.URL.Query().Get("project")
	name, _ := url.PathUnescape(strings.TrimPrefix(request.URL.Path, "/1.0/profiles/"))
	fake.mu.Lock()
	profile, ok := fake.profiles[project+"/"+name]
	fake.mu.Unlock()
	if !ok {
		writeNetworkIncusError(writer, http.StatusNotFound, "profile not found")
		return
	}
	writer.Header().Set("ETag", profile.ETag)
	writeNetworkIncusSync(writer, profileMetadata(project, name, profile))
}

func profileMetadata(project, name string, profile fakeProfile) map[string]any {
	return map[string]any{
		"name": name, "project": project, "description": profile.Description,
		"config": profile.Config, "devices": profile.Devices, "used_by": profile.UsedBy,
	}
}

func (fake *networkIncusServer) getInstances(writer http.ResponseWriter, request *http.Request) {
	project := request.URL.Query().Get("project")
	fake.mu.Lock()
	result := make([]map[string]any, 0)
	for key, instance := range fake.instances {
		candidateProject, _, _ := strings.Cut(key, "/")
		if candidateProject == project {
			result = append(result, instance.Data)
		}
	}
	fake.mu.Unlock()
	writeNetworkIncusSync(writer, result)
}

func (fake *networkIncusServer) getInstance(writer http.ResponseWriter, request *http.Request) {
	project := request.URL.Query().Get("project")
	name, _ := url.PathUnescape(strings.TrimPrefix(request.URL.Path, "/1.0/instances/"))
	fake.mu.Lock()
	instance, ok := fake.instances[project+"/"+name]
	fake.mu.Unlock()
	if !ok {
		writeNetworkIncusError(writer, http.StatusNotFound, "instance not found")
		return
	}
	writer.Header().Set("ETag", instance.ETag)
	writeNetworkIncusSync(writer, instance.Data)
}

func (fake *networkIncusServer) getInstanceState(writer http.ResponseWriter, request *http.Request) {
	project := request.URL.Query().Get("project")
	name := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/1.0/instances/"), "/state")
	fake.mu.Lock()
	instance, ok := fake.instances[project+"/"+name]
	fake.mu.Unlock()
	if !ok || instance.State == nil {
		writeNetworkIncusError(writer, http.StatusNotFound, "instance state not found")
		return
	}
	writeNetworkIncusSync(writer, instance.State)
}

func (fake *networkIncusServer) getACL(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Query().Get("project") != defaultProject {
		writeNetworkIncusError(writer, http.StatusBadRequest, "ACL must use default project")
		return
	}
	name, _ := url.PathUnescape(strings.TrimPrefix(request.URL.Path, "/1.0/network-acls/"))
	fake.mu.Lock()
	acl, ok := fake.acls[name]
	fake.mu.Unlock()
	if !ok {
		writeNetworkIncusError(writer, http.StatusNotFound, "ACL not found")
		return
	}
	writer.Header().Set("ETag", acl.ETag)
	writeNetworkIncusSync(writer, map[string]any{
		"name": name, "config": acl.Config, "ingress": acl.Ingress, "egress": acl.Egress,
	})
}

func (fake *networkIncusServer) createACL(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Query().Get("project") != defaultProject {
		writeNetworkIncusError(writer, http.StatusBadRequest, "ACL must use default project")
		return
	}
	var body struct {
		Name    string            `json:"name"`
		Config  map[string]string `json:"config"`
		Ingress []map[string]any  `json:"ingress"`
		Egress  []map[string]any  `json:"egress"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeNetworkIncusError(writer, http.StatusBadRequest, "bad ACL")
		return
	}
	fake.mu.Lock()
	if _, exists := fake.acls[body.Name]; exists {
		fake.mu.Unlock()
		writeNetworkIncusError(writer, http.StatusConflict, "ACL exists")
		return
	}
	fake.acls[body.Name] = fakeACL{ETag: "acl-v1", Config: body.Config, Ingress: body.Ingress, Egress: body.Egress}
	fake.mu.Unlock()
	writeNetworkIncusSync(writer, map[string]any{})
}

func (fake *networkIncusServer) updateACL(writer http.ResponseWriter, request *http.Request) {
	name, _ := url.PathUnescape(strings.TrimPrefix(request.URL.Path, "/1.0/network-acls/"))
	var body struct {
		Config  map[string]string `json:"config"`
		Ingress []map[string]any  `json:"ingress"`
		Egress  []map[string]any  `json:"egress"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeNetworkIncusError(writer, http.StatusBadRequest, "bad ACL")
		return
	}
	fake.mu.Lock()
	fake.aclIfMatch = request.Header.Get("If-Match")
	acl, exists := fake.acls[name]
	if !exists || fake.aclIfMatch != acl.ETag {
		fake.mu.Unlock()
		writeNetworkIncusError(writer, http.StatusPreconditionFailed, "stale ACL")
		return
	}
	acl.Config, acl.Ingress, acl.Egress, acl.ETag = body.Config, body.Ingress, body.Egress, "acl-v2"
	fake.acls[name] = acl
	fake.mu.Unlock()
	writeNetworkIncusSync(writer, map[string]any{})
}

func (fake *networkIncusServer) deleteACL(writer http.ResponseWriter, request *http.Request) {
	name, _ := url.PathUnescape(strings.TrimPrefix(request.URL.Path, "/1.0/network-acls/"))
	fake.mu.Lock()
	_, exists := fake.acls[name]
	if exists {
		delete(fake.acls, name)
	}
	fake.mu.Unlock()
	if !exists {
		writeNetworkIncusError(writer, http.StatusNotFound, "ACL not found")
		return
	}
	writeNetworkIncusSync(writer, map[string]any{})
}

func (fake *networkIncusServer) updateProfile(writer http.ResponseWriter, request *http.Request) {
	project := request.URL.Query().Get("project")
	name, _ := url.PathUnescape(strings.TrimPrefix(request.URL.Path, "/1.0/profiles/"))
	var body struct {
		Description string                       `json:"description"`
		Config      map[string]string            `json:"config"`
		Devices     map[string]map[string]string `json:"devices"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeNetworkIncusError(writer, http.StatusBadRequest, "bad profile")
		return
	}
	fake.mu.Lock()
	fake.profileIfMatch = request.Header.Get("If-Match")
	profile, exists := fake.profiles[project+"/"+name]
	if !exists || fake.profileIfMatch != profile.ETag {
		fake.mu.Unlock()
		writeNetworkIncusError(writer, http.StatusPreconditionFailed, "stale profile")
		return
	}
	profile.Description, profile.Config, profile.Devices, profile.ETag = body.Description, body.Config, body.Devices, "profile-v2"
	fake.profiles[project+"/"+name] = profile
	fake.mu.Unlock()
	writeNetworkIncusSync(writer, map[string]any{})
}

func (fake *networkIncusServer) updatePower(writer http.ResponseWriter, request *http.Request) {
	project := request.URL.Query().Get("project")
	name := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/1.0/instances/"), "/state")
	var body struct {
		Action  string `json:"action"`
		Timeout int    `json:"timeout"`
		Force   bool   `json:"force"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeNetworkIncusError(writer, http.StatusBadRequest, "bad power request")
		return
	}
	fake.mu.Lock()
	fake.powerCalls = append(fake.powerCalls, fakePowerCall{
		Project: project, Name: name, Action: body.Action, Timeout: body.Timeout, Force: body.Force,
	})
	fake.nextOperation++
	id := fmt.Sprintf("network-operation-%d", fake.nextOperation)
	fake.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"type": "async", "status": "Operation created", "status_code": http.StatusAccepted,
		"operation": "/1.0/operations/" + id,
		"metadata":  map[string]any{"id": id, "class": "task", "status": "Running", "status_code": 103},
	})
}

func writeNetworkIncusSync(writer http.ResponseWriter, metadata any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"type": "sync", "status": "Success", "status_code": http.StatusOK,
		"metadata": metadata,
	})
}

func writeNetworkIncusError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"type": "error", "error": message, "error_code": status,
	})
}
