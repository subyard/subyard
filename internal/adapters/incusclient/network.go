package incusclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/yardnetwork"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

const defaultProject = "default"

var _ yardnetwork.Host = (*Client)(nil)

func (client *Client) ReadPolicy(ctx context.Context) (yardnetwork.StoredPolicy, error) {
	server, err := client.networkServer(ctx)
	if err != nil {
		return yardnetwork.StoredPolicy{}, err
	}
	return readPolicy(server)
}

func (client *Client) WritePolicy(
	ctx context.Context,
	observed yardnetwork.StoredPolicy,
	desired yardnetwork.Policy,
) (yardnetwork.StoredPolicy, error) {
	if observed.ETag == "" {
		return yardnetwork.StoredPolicy{}, errors.New("network policy observation ETag is required")
	}
	content, err := yardnetwork.Encode(desired)
	if err != nil {
		return yardnetwork.StoredPolicy{}, err
	}
	server, err := client.networkServer(ctx)
	if err != nil {
		return yardnetwork.StoredPolicy{}, err
	}
	project, _, err := server.GetProject(defaultProject)
	if err != nil {
		return yardnetwork.StoredPolicy{}, normalizeError("get default project for network policy", err)
	}
	project.Config = cloneMap(project.Config)
	project.Config[yardnetwork.PolicyKey] = string(content)
	if err := server.UpdateProject(defaultProject, project.Writable(), observed.ETag); err != nil {
		return yardnetwork.StoredPolicy{}, normalizeError("update network policy", err)
	}
	return readPolicy(server)
}

func readPolicy(server incus.InstanceServer) (yardnetwork.StoredPolicy, error) {
	project, etag, err := server.GetProject(defaultProject)
	if err != nil {
		return yardnetwork.StoredPolicy{}, normalizeError("get default project network policy", err)
	}
	content := project.Config[yardnetwork.PolicyKey]
	policy, err := yardnetwork.Decode([]byte(content))
	if err != nil {
		return yardnetwork.StoredPolicy{}, err
	}
	return yardnetwork.StoredPolicy{Policy: policy, Content: content, ETag: etag}, nil
}

func (client *Client) InspectNetwork(
	ctx context.Context,
	yards []yardnetwork.Yard,
) (yardnetwork.Snapshot, error) {
	server, err := client.networkServer(ctx)
	if err != nil {
		return yardnetwork.Snapshot{}, err
	}
	info, _, err := server.GetServer()
	if err != nil {
		return yardnetwork.Snapshot{}, normalizeError("get server network environment", err)
	}
	snapshot := yardnetwork.Snapshot{
		Firewall: info.Environment.Firewall, Clustered: info.Environment.ServerClustered,
		Networks:          make(map[string]yardnetwork.Network),
		ReservationClaims: make(map[string]map[string][]yardnetwork.ReservationClaim),
	}
	yardByName := make(map[string]yardnetwork.Yard, len(yards))
	for _, yard := range yards {
		if existing, found := yardByName[yard.Name]; found && existing != yard {
			return yardnetwork.Snapshot{}, fmt.Errorf("requested network yard name %q has conflicting identities", yard.Name)
		}
		yardByName[yard.Name] = yard
	}
	defaultServer := server.UseProject(defaultProject)
	networks, err := defaultServer.GetNetworks()
	if err != nil {
		return yardnetwork.Snapshot{}, normalizeError("list default project networks", err)
	}
	leasesByProjectNetwork := make(map[string][]api.NetworkLease)
	for index := range networks {
		network := networks[index]
		if !network.Managed {
			continue
		}
		snapshot.Networks[network.Name] = yardnetwork.Network{
			Name: network.Name, Type: network.Type, Managed: true, Config: cloneMap(network.Config),
		}
	}
	projects, err := server.GetProjects()
	if err != nil {
		return yardnetwork.Snapshot{}, normalizeError("list projects for network reservations", err)
	}
	for index := range projects {
		project := projects[index]
		projectServer := server.UseProject(project.Name)
		usesDefaultNetworks := project.Name == defaultProject || project.Config["features.networks"] != "true"
		profiles, err := projectServer.GetProfiles()
		if err != nil {
			return yardnetwork.Snapshot{}, normalizeError(
				"list profiles for network reservations in project "+project.Name, err)
		}
		for profileIndex := range profiles {
			if usesDefaultNetworks {
				addProfileReservations(snapshot.ReservationClaims, project.Name, &profiles[profileIndex])
			}
		}
		instances, err := projectServer.GetInstances(api.InstanceTypeAny)
		if err != nil {
			return yardnetwork.Snapshot{}, normalizeError(
				"list instances for network reservations in project "+project.Name, err)
		}
		for instanceIndex := range instances {
			if usesDefaultNetworks {
				addInstanceReservations(snapshot.ReservationClaims, project.Name, &instances[instanceIndex])
			}
			if err := addManagedNetworkYard(yardByName, project.Name, &instances[instanceIndex]); err != nil {
				return yardnetwork.Snapshot{}, err
			}
		}
		// Projects with their own network namespace cannot see or attach the
		// default project's managed bridges.
		if !usesDefaultNetworks {
			continue
		}
		visibleNetworks, err := projectServer.GetNetworks()
		if err != nil {
			return yardnetwork.Snapshot{}, normalizeError(
				"list visible networks for DHCP reservations in project "+project.Name, err)
		}
		for visibleIndex := range visibleNetworks {
			networkName := visibleNetworks[visibleIndex].Name
			if _, isDefaultManaged := snapshot.Networks[networkName]; !isDefaultManaged {
				continue
			}
			leases, err := projectServer.GetNetworkLeases(networkName)
			if err != nil {
				return yardnetwork.Snapshot{}, normalizeError(
					"get network leases for "+networkName+" in project "+project.Name, err)
			}
			leasesByProjectNetwork[projectNetworkKey(project.Name, networkName)] = leases
			for leaseIndex := range leases {
				claims := leaseReservationClaims(project.Name, networkName, &leases[leaseIndex], instances)
				if len(claims) == 0 {
					claims = []yardnetwork.ReservationClaim{{Project: project.Name}}
				}
				for _, claim := range claims {
					addReservationClaim(snapshot.ReservationClaims, networkName, leases[leaseIndex].Address, claim)
				}
			}
		}
	}
	mergedYards := make([]yardnetwork.Yard, 0, len(yardByName))
	for _, yard := range yardByName {
		mergedYards = append(mergedYards, yard)
	}
	sort.Slice(mergedYards, func(left, right int) bool { return mergedYards[left].Name < mergedYards[right].Name })
	snapshot.Yards = make([]yardnetwork.ObservedYard, 0, len(mergedYards))
	for _, yard := range mergedYards {
		observed, err := inspectYard(server, yard,
			leasesByProjectNetwork[projectNetworkKey(yard.Project, yard.Network)])
		if err != nil {
			return yardnetwork.Snapshot{}, err
		}
		snapshot.Yards = append(snapshot.Yards, observed)
	}
	for _, byAddress := range snapshot.ReservationClaims {
		for address := range byAddress {
			sort.Slice(byAddress[address], func(left, right int) bool {
				return reservationClaimKey(byAddress[address][left]) < reservationClaimKey(byAddress[address][right])
			})
		}
	}
	return snapshot, nil
}

func addManagedNetworkYard(
	yards map[string]yardnetwork.Yard,
	project string,
	instance *api.Instance,
) error {
	managed := effectiveInstanceConfig(instance, "user.subyard.managed")
	if managed == "" {
		return nil
	}
	if managed != "true" {
		return fmt.Errorf("managed network metadata for %s/%s has invalid marker %q", project, instance.Name, managed)
	}
	yard := yardnetwork.Yard{
		Name: effectiveInstanceConfig(instance, "user.subyard.name"), Project: project,
		Instance: instance.Name, Network: effectiveInstanceConfig(instance, "user.subyard.bridge"),
	}
	if !domain.SafeName(yard.Name) || !domain.SafeName(yard.Project) ||
		!domain.SafeName(yard.Instance) || !domain.SafeName(yard.Network) {
		return fmt.Errorf("managed network metadata for %s/%s has an invalid yard identity", project, instance.Name)
	}
	if existing, found := yards[yard.Name]; found && existing != yard {
		return fmt.Errorf("managed network metadata for yard %q conflicts between %s/%s and %s/%s",
			yard.Name, existing.Project, existing.Instance, yard.Project, yard.Instance)
	}
	yards[yard.Name] = yard
	return nil
}

func effectiveInstanceConfig(instance *api.Instance, key string) string {
	if value, found := instance.ExpandedConfig[key]; found {
		return value
	}
	return instance.Config[key]
}

func inspectYard(
	server incus.InstanceServer,
	yard yardnetwork.Yard,
	leases []api.NetworkLease,
) (yardnetwork.ObservedYard, error) {
	observed := yardnetwork.ObservedYard{Yard: yard}
	project, etag, err := server.GetProject(yard.Project)
	if err != nil {
		if !api.StatusErrorCheck(err, http.StatusNotFound) {
			return observed, normalizeError("get yard project "+yard.Project, err)
		}
	} else {
		observed.ProjectFound = true
		observed.ProjectETag = etag
		observed.ProjectConfig = cloneMap(project.Config)
		projectServer := server.UseProject(yard.Project)
		profile, profileETag, err := projectServer.GetProfile(defaultProject)
		if err != nil {
			if !api.StatusErrorCheck(err, http.StatusNotFound) {
				return observed, normalizeError("get yard default profile "+yard.Project, err)
			}
		} else {
			observed.ProfileFound = true
			observed.ProfileETag = profileETag
			observed.ProfileDevices = cloneDevices(profile.Devices)
			observed.ProfileUsedBy = slices.Clone(profile.UsedBy)
		}
		instance, _, err := projectServer.GetInstance(yard.Instance)
		if err != nil {
			if !api.StatusErrorCheck(err, http.StatusNotFound) {
				return observed, normalizeError("get yard instance "+yard.Project+"/"+yard.Instance, err)
			}
		} else {
			observed.InstanceFound = true
			observed.InstanceInfo = instanceInfo(yard.Project, instance)
			state, _, err := projectServer.GetInstanceState(yard.Instance)
			if err != nil {
				return observed, normalizeError("get yard instance state "+yard.Project+"/"+yard.Instance, err)
			}
			observed.MAC, observed.IPv4 = liveNIC(instance, state, leases)
		}
	}
	aclName := yardnetwork.ACLName(yard)
	observed.ACL.Name = aclName
	acl, aclETag, err := server.UseProject(defaultProject).GetNetworkACL(aclName)
	if err != nil {
		if api.StatusErrorCheck(err, http.StatusNotFound) {
			return observed, nil
		}
		return observed, normalizeError("get yard network ACL "+aclName, err)
	}
	observed.ACL.Exists = true
	observed.ACL.ETag = aclETag
	observed.ACL.Config = cloneMap(acl.Config)
	observed.ACL.Ingress = networkRules(acl.Ingress)
	observed.ACL.Egress = networkRules(acl.Egress)
	return observed, nil
}

func liveNIC(instance *api.Instance, state *api.InstanceState, leases []api.NetworkLease) (string, string) {
	device := instance.ExpandedDevices["eth0"]
	interfaceName := device["name"]
	if interfaceName == "" {
		interfaceName = "eth0"
	}
	mac := instance.Config["volatile.eth0.hwaddr"]
	if mac == "" {
		mac = instance.ExpandedConfig["volatile.eth0.hwaddr"]
	}
	if mac == "" {
		mac = device["hwaddr"]
	}
	stateNIC, found := state.Network[interfaceName]
	if !found && interfaceName != "eth0" {
		stateNIC, found = state.Network["eth0"]
	}
	if found {
		if mac == "" {
			mac = stateNIC.Hwaddr
		}
		for _, address := range stateNIC.Addresses {
			if address.Family == "inet" && address.Scope != "link" && net.ParseIP(address.Address).To4() != nil {
				return mac, address.Address
			}
		}
	}
	for _, lease := range leases {
		if net.ParseIP(lease.Address).To4() == nil {
			continue
		}
		if mac != "" && strings.EqualFold(lease.Hwaddr, mac) {
			return mac, lease.Address
		}
		if lease.Hostname == instance.Name {
			return mac, lease.Address
		}
	}
	return mac, ""
}

func networkRules(source []api.NetworkACLRule) []yardnetwork.Rule {
	result := make([]yardnetwork.Rule, len(source))
	for index, rule := range source {
		result[index] = yardnetwork.Rule{
			Action: rule.Action, State: rule.State, Source: rule.Source, Destination: rule.Destination,
			Protocol: rule.Protocol, DestinationPort: rule.DestinationPort,
		}
	}
	return result
}

func addProfileReservations(
	claims map[string]map[string][]yardnetwork.ReservationClaim,
	project string,
	profile *api.Profile,
) {
	users := profileInstanceClaims(project, profile.UsedBy)
	for _, device := range profile.Devices {
		if device["type"] != "nic" {
			continue
		}
		networkName := deviceNetwork(device)
		deviceClaims := users
		if len(deviceClaims) == 0 {
			deviceClaims = []yardnetwork.ReservationClaim{{Project: project, Profile: profile.Name}}
		}
		for _, claim := range deviceClaims {
			claim.MAC = canonicalMAC(device["hwaddr"])
			addReservationClaim(claims, networkName, device["ipv4.address"], claim)
		}
	}
}

func addInstanceReservations(
	claims map[string]map[string][]yardnetwork.ReservationClaim,
	project string,
	instance *api.Instance,
) {
	for name, device := range instance.Devices {
		if device["type"] != "nic" {
			continue
		}
		mac := device["hwaddr"]
		if mac == "" {
			mac = instance.Config["volatile."+name+".hwaddr"]
		}
		addReservationClaim(claims, deviceNetwork(device), device["ipv4.address"],
			yardnetwork.ReservationClaim{Project: project, Instance: instance.Name, MAC: canonicalMAC(mac)})
	}
}

func profileInstanceClaims(project string, usedBy []string) []yardnetwork.ReservationClaim {
	result := []yardnetwork.ReservationClaim{}
	seen := map[string]bool{}
	for _, reference := range usedBy {
		parsed, err := url.Parse(reference)
		if err != nil || !strings.HasPrefix(parsed.Path, "/1.0/instances/") {
			continue
		}
		instance, err := url.PathUnescape(strings.TrimPrefix(parsed.Path, "/1.0/instances/"))
		if err != nil || instance == "" || strings.Contains(instance, "/") {
			continue
		}
		instanceProject := parsed.Query().Get("project")
		if instanceProject == "" {
			instanceProject = project
		}
		key := instanceProject + "\x00" + instance
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, yardnetwork.ReservationClaim{Project: instanceProject, Instance: instance})
	}
	return result
}

func leaseReservationClaims(
	project string,
	networkName string,
	lease *api.NetworkLease,
	instances []api.Instance,
) []yardnetwork.ReservationClaim {
	mac := canonicalMAC(lease.Hwaddr)
	if mac == "" {
		return []yardnetwork.ReservationClaim{{Project: project}}
	}
	result := []yardnetwork.ReservationClaim{}
	for index := range instances {
		for deviceName, device := range instances[index].ExpandedDevices {
			if device["type"] != "nic" || deviceNetwork(device) != networkName {
				continue
			}
			deviceMAC := device["hwaddr"]
			if deviceMAC == "" {
				deviceMAC = instances[index].ExpandedConfig["volatile."+deviceName+".hwaddr"]
			}
			if canonicalMAC(deviceMAC) == mac {
				result = append(result, yardnetwork.ReservationClaim{
					Project: project, Instance: instances[index].Name, MAC: mac,
				})
				break
			}
		}
	}
	if len(result) == 0 {
		result = append(result, yardnetwork.ReservationClaim{Project: project, MAC: mac})
	}
	return result
}

func addReservationClaim(
	claims map[string]map[string][]yardnetwork.ReservationClaim,
	networkName string,
	value string,
	claim yardnetwork.ReservationClaim,
) {
	if networkName == "" {
		return
	}
	address := net.ParseIP(value)
	if address == nil {
		if parsed, _, err := net.ParseCIDR(value); err == nil {
			address = parsed
		}
	}
	if address == nil || address.To4() == nil {
		return
	}
	canonicalAddress := address.String()
	if claims[networkName] == nil {
		claims[networkName] = make(map[string][]yardnetwork.ReservationClaim)
	}
	claim.MAC = canonicalMAC(claim.MAC)
	for index, existing := range claims[networkName][canonicalAddress] {
		if !sameReservationOwner(existing, claim) {
			continue
		}
		if existing.MAC == "" {
			claims[networkName][canonicalAddress][index].MAC = claim.MAC
			return
		}
		if claim.MAC == "" || existing.MAC == claim.MAC {
			return
		}
	}
	claims[networkName][canonicalAddress] = append(claims[networkName][canonicalAddress], claim)
}

func sameReservationOwner(left, right yardnetwork.ReservationClaim) bool {
	if left.Project != right.Project {
		return false
	}
	if left.Instance != "" || right.Instance != "" {
		return left.Instance != "" && left.Instance == right.Instance
	}
	if left.Profile != "" || right.Profile != "" {
		return left.Profile != "" && left.Profile == right.Profile
	}
	return left.MAC == right.MAC
}

func reservationClaimKey(claim yardnetwork.ReservationClaim) string {
	return claim.Project + "\x00" + claim.Instance + "\x00" + claim.Profile + "\x00" + claim.MAC
}

func deviceNetwork(device map[string]string) string {
	networkName := device["network"]
	if networkName == "" {
		networkName = device["parent"]
	}
	return networkName
}

func canonicalMAC(value string) string {
	mac, err := net.ParseMAC(value)
	if err != nil || len(mac) != 6 {
		return ""
	}
	return mac.String()
}

func projectNetworkKey(project, networkName string) string {
	return project + "\x00" + networkName
}

func (client *Client) WriteNetworkACL(ctx context.Context, acl yardnetwork.ACL) error {
	if acl.Name == "" {
		return errors.New("network ACL name is required")
	}
	if acl.Exists && acl.ETag == "" {
		return errors.New("existing network ACL observation ETag is required")
	}
	server, err := client.networkServer(ctx)
	if err != nil {
		return err
	}
	projectServer := server.UseProject(defaultProject)
	desired := api.NetworkACLPut{
		Config: cloneMap(acl.Config), Ingress: apiNetworkRules(acl.Ingress), Egress: apiNetworkRules(acl.Egress),
	}
	if !acl.Exists {
		err := projectServer.CreateNetworkACL(api.NetworkACLsPost{
			NetworkACLPost: api.NetworkACLPost{Name: acl.Name}, NetworkACLPut: desired,
		})
		if err != nil {
			return normalizeError("create network ACL "+acl.Name, err)
		}
		return nil
	}
	if err := projectServer.UpdateNetworkACL(acl.Name, desired, acl.ETag); err != nil {
		return normalizeError("update network ACL "+acl.Name, err)
	}
	return nil
}

func (client *Client) DeleteNetworkACL(ctx context.Context, observed yardnetwork.ACL) error {
	if !observed.Exists {
		return nil
	}
	if observed.Name == "" || observed.ETag == "" {
		return errors.New("existing network ACL observation requires a name and ETag")
	}
	server, err := client.networkServer(ctx)
	if err != nil {
		return err
	}
	projectServer := server.UseProject(defaultProject)
	_, currentETag, err := projectServer.GetNetworkACL(observed.Name)
	if err != nil {
		return normalizeError("recheck network ACL "+observed.Name, err)
	}
	if currentETag != observed.ETag {
		return fmt.Errorf("network ACL %q changed since inspection", observed.Name)
	}
	// Incus' typed DeleteNetworkACL endpoint has no ETag parameter. Recheck the
	// exact object immediately before the exact delete so stale observations fail.
	if err := projectServer.DeleteNetworkACL(observed.Name); err != nil {
		return normalizeError("delete network ACL "+observed.Name, err)
	}
	return nil
}

func (client *Client) WriteProfileNIC(
	ctx context.Context,
	observed yardnetwork.ObservedYard,
	nic map[string]string,
) error {
	if !observed.ProfileFound || observed.ProfileETag == "" {
		return errors.New("default profile observation with ETag is required")
	}
	server, err := client.networkServer(ctx)
	if err != nil {
		return err
	}
	projectServer := server.UseProject(observed.Project)
	profile, _, err := projectServer.GetProfile(defaultProject)
	if err != nil {
		return normalizeError("get default profile for network update", err)
	}
	profile.Devices = cloneDevices(profile.Devices)
	profile.Devices["eth0"] = cloneMap(nic)
	if err := projectServer.UpdateProfile(defaultProject, profile.Writable(), observed.ProfileETag); err != nil {
		return normalizeError("update default profile network device", err)
	}
	return nil
}

func (client *Client) WriteProjectNetworks(
	ctx context.Context,
	observed yardnetwork.ObservedYard,
	networks string,
) error {
	if !observed.ProjectFound || observed.ProjectETag == "" {
		return errors.New("project observation with ETag is required")
	}
	server, err := client.networkServer(ctx)
	if err != nil {
		return err
	}
	project, _, err := server.GetProject(observed.Project)
	if err != nil {
		return normalizeError("get project for network access update", err)
	}
	project.Config = cloneMap(project.Config)
	if networks == "" {
		delete(project.Config, "restricted.networks.access")
	} else {
		project.Config["restricted.networks.access"] = networks
	}
	if err := server.UpdateProject(observed.Project, project.Writable(), observed.ProjectETag); err != nil {
		return normalizeError("update project network access", err)
	}
	return nil
}

func (client *Client) NetworkPower(ctx context.Context, yard yardnetwork.Yard, action string) error {
	if action != "start" && action != "stop" {
		return fmt.Errorf("invalid network power action %q", action)
	}
	powerContext, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	server, err := client.networkServer(powerContext)
	if err != nil {
		return err
	}
	operation, err := server.UseProject(yard.Project).UpdateInstanceState(yard.Instance, api.InstanceStatePut{
		Action: action, Timeout: 30, Force: false,
	}, "")
	if err != nil {
		return normalizeError(action+" network-managed instance", err)
	}
	if err := operation.WaitContext(powerContext); err != nil {
		return normalizeError("wait for "+action+" network-managed instance", err)
	}
	return nil
}

func apiNetworkRules(source []yardnetwork.Rule) []api.NetworkACLRule {
	result := make([]api.NetworkACLRule, len(source))
	for index, rule := range source {
		result[index] = api.NetworkACLRule{
			Action: rule.Action, State: rule.State, Source: rule.Source, Destination: rule.Destination,
			Protocol: rule.Protocol, DestinationPort: rule.DestinationPort,
		}
	}
	return result
}

func (client *Client) networkServer(ctx context.Context) (incus.InstanceServer, error) {
	server, err := client.connect(ctx, true)
	if err != nil {
		return nil, err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return nil, err
	}
	return server, nil
}
