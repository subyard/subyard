package yardnetwork

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/Subyard/Subyard/internal/domain"
)

func planDigest(value any) string {
	payload, err := json.Marshal(value)
	if err != nil {
		panic("network plan must be serializable")
	}
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

func normalizedDesiredPolicy(policy Policy) Policy {
	payload, _ := json.Marshal(policy)
	var copy Policy
	_ = json.Unmarshal(payload, &copy)
	copy.Revision, copy.AppliedRevision = 0, 0
	copy.AppliedIsolation, copy.PendingStart, copy.Removing = false, nil, nil
	if !copy.Isolation {
		copy.Bindings = nil
	}
	return copy
}

func networkYardGuard(yard ObservedYard) string {
	payload, _ := json.Marshal(yard)
	var copy ObservedYard
	_ = json.Unmarshal(payload, &copy)
	copy.ProjectETag, copy.ProfileETag, copy.ACL = "", "", ACL{}
	delete(copy.ProjectConfig, "restricted.networks.access")
	nic := copy.ProfileDevices["eth0"]
	for _, key := range []string{"name", "hwaddr", "ipv4.address", "security.mac_filtering", "security.ipv4_filtering", "security.acls", "security.acls.default.ingress.action", "security.acls.default.egress.action"} {
		delete(nic, key)
	}
	if device := copy.InstanceInfo.Devices["eth0"]; device != nil {
		for _, key := range []string{"name", "hwaddr", "ipv4.address", "security.mac_filtering", "security.ipv4_filtering", "security.acls", "security.acls.default.ingress.action", "security.acls.default.egress.action"} {
			delete(device, key)
		}
	}
	return planDigest(copy)
}

// CheckApproved accepts physical convergence within the approved yard set. It
// retains native ownership, address-claim and permission validation in Prepare.
func CheckApproved(approved, fresh Plan) error {
	if approved.Fingerprint == fresh.Fingerprint {
		return nil
	}
	if !approved.Changed {
		return fmt.Errorf("%w: converged network observation changed", domain.ErrPlanStale)
	}
	if !slices.Equal(approved.Yards, fresh.Yards) || !reflect.DeepEqual(normalizedDesiredPolicy(approved.Policy), normalizedDesiredPolicy(fresh.Policy)) {
		return fmt.Errorf("%w: network desired policy or yard set changed", domain.ErrPlanStale)
	}
	if approved.Snapshot.Firewall != fresh.Snapshot.Firewall || approved.Snapshot.Clustered != fresh.Snapshot.Clustered || !reflect.DeepEqual(approved.Snapshot.Networks, fresh.Snapshot.Networks) || !reflect.DeepEqual(approved.Snapshot.ReservationClaims, fresh.Snapshot.ReservationClaims) {
		return fmt.Errorf("%w: network infrastructure or address ownership changed", domain.ErrPlanStale)
	}
	before := make(map[Yard]ObservedYard)
	allowed := make(map[Yard]Update)
	for _, update := range approved.Updates {
		allowed[update.Yard.Yard] = update
	}
	for _, yard := range approved.Snapshot.Yards {
		before[yard.Yard] = yard
	}
	for _, yard := range fresh.Snapshot.Yards {
		old, exists := before[yard.Yard]
		if !exists || networkYardGuard(old) != networkYardGuard(yard) {
			return fmt.Errorf("%w: network yard ownership changed", domain.ErrPlanStale)
		}
		if update, needsWork := allowed[yard.Yard]; needsWork {
			if !networkMapConverges(old.ProfileDevices["eth0"], yard.ProfileDevices["eth0"], update.NIC) || !networkMapConverges(old.InstanceInfo.Devices["eth0"], yard.InstanceInfo.Devices["eth0"], update.NIC) ||
				(old.ProjectConfig["restricted.networks.access"] != yard.ProjectConfig["restricted.networks.access"] && yard.ProjectConfig["restricted.networks.access"] != update.Access) {
				return fmt.Errorf("%w: native network fields changed outside approved convergence", domain.ErrPlanStale)
			}
			aclChanged := old.ACL.Exists != yard.ACL.Exists || !maps.Equal(old.ACL.Config, yard.ACL.Config) || !reflect.DeepEqual(old.ACL.Ingress, yard.ACL.Ingress) || !reflect.DeepEqual(old.ACL.Egress, yard.ACL.Egress)
			if aclChanged && !(!yard.ACL.Exists && update.RemoveACL) && !sameACL(yard.ACL, update.ACL) {
				return fmt.Errorf("%w: native network ACL changed outside approved convergence", domain.ErrPlanStale)
			}
			if !aclChanged && old.ACL.ETag != yard.ACL.ETag {
				return fmt.Errorf("%w: native ACL revision changed without approved convergence", domain.ErrPlanStale)
			}
		}
		if (reflect.DeepEqual(old.ProfileDevices, yard.ProfileDevices) && old.ProfileETag != yard.ProfileETag) || (reflect.DeepEqual(old.ProjectConfig, yard.ProjectConfig) && old.ProjectETag != yard.ProjectETag) {
			return fmt.Errorf("%w: native network target revision changed without approved convergence", domain.ErrPlanStale)
		}
	}
	for _, update := range fresh.Updates {
		old, exists := allowed[update.Yard.Yard]
		if !exists || old.Access != update.Access || old.RemoveACL != update.RemoveACL || !maps.Equal(old.NIC, update.NIC) || !maps.Equal(old.ACL.Config, update.ACL.Config) || !reflect.DeepEqual(old.ACL.Ingress, update.ACL.Ingress) || !reflect.DeepEqual(old.ACL.Egress, update.ACL.Egress) {
			return fmt.Errorf("%w: network target needs unapproved work", domain.ErrPlanStale)
		}
	}
	return nil
}

func networkMapConverges(before, fresh, desired map[string]string) bool {
	keys := maps.Clone(before)
	if keys == nil {
		keys = make(map[string]string)
	}
	maps.Copy(keys, fresh)
	for key := range keys {
		old, oldPresent := before[key]
		current, currentPresent := fresh[key]
		if old == current && oldPresent == currentPresent {
			continue
		}
		want, wantPresent := desired[key]
		if current != want || currentPresent != wantPresent {
			return false
		}
	}
	return true
}

func (approved Plan) OperationSteps(fresh Plan) []domain.OperationStep {
	decision := domain.StepSkip
	if fresh.Changed {
		decision = domain.StepApply
	}
	steps := []domain.OperationStep{{ID: "network.policy", Target: "owner host registered yard network policy",
		Observed: "captured native policy revision", Desired: "fingerprint:" + planDigest(normalizedDesiredPolicy(approved.Policy)), Decision: decision,
		Preconditions: []string{"captured native policy, registered yard set, ownership and address claims remain valid"}, Verify: "read the saved policy and prove its desired and applied revisions agree", Consequence: "save the approved host network isolation and explicit links"}}
	pending := make(map[Yard]bool)
	for _, update := range fresh.Updates {
		pending[update.Yard.Yard] = true
	}
	for _, update := range approved.Updates {
		decision := domain.StepSkip
		if pending[update.Yard.Yard] {
			decision = domain.StepApply
		}
		desired := planDigest(struct {
			NIC    map[string]string
			Access string
			ACL    ACL
			Remove bool
		}{update.NIC, update.Access, ACL{Config: update.ACL.Config, Ingress: update.ACL.Ingress, Egress: update.ACL.Egress}, update.RemoveACL})
		power := update.Yard.InstanceInfo.Status
		if power == "" {
			power = "captured power state"
		}
		steps = append(steps, domain.OperationStep{ID: "network.yard." + update.Yard.Name, Target: update.Yard.Project + "/" + update.Yard.Instance,
			Observed: "captured native network and power state", Desired: "network fingerprint:" + desired + "; restore " + power, Decision: decision,
			Preconditions: []string{"native NIC, ACL, project and instance ownership remain valid", "host network safety guard passes"}, DependsOn: []string{"network.policy"},
			Verify: "inspect approved NIC, ACL and project settings and verify original running state is restored", Consequence: "briefly stop and restore " + update.Yard.Name + " while applying its approved network settings; active connections close"})
	}
	for _, name := range approved.Policy.PendingStart {
		decision := domain.StepSkip
		if slices.Contains(fresh.Policy.PendingStart, name) {
			decision = domain.StepApply
		}
		steps = append(steps, domain.OperationStep{ID: "network.restart." + name, Target: "registered local yard " + name, Observed: "captured pending native restart", Desired: "running with approved network policy", Decision: decision, DependsOn: []string{"network.policy"}, Preconditions: []string{"native yard identity and host safety guards pass"}, Verify: "inspect actual instance power state", Consequence: "restore pending yard " + name + " to running"})
	}
	return steps
}

func (plan Plan) StateBinding() string {
	return planDigest(struct {
		Plan     Plan
		Snapshot Snapshot
	}{plan, plan.Snapshot})
}
