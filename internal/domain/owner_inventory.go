package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const OwnerInventorySchema = 1

type OwnerProject struct {
	ProjectID       string `json:"projectId"`
	IdentityVersion int    `json:"identityVersion,omitempty"`
	Name            string `json:"name"`
	Mode            string `json:"mode"`
	Target          string `json:"target"`
	SourceKey       string `json:"sourceKey,omitempty"`
}

type OwnerYard struct {
	Name              string            `json:"name"`
	Kind              string            `json:"kind"`
	Instance          string            `json:"instance"`
	State             string            `json:"state"`
	SSHPort           int               `json:"sshPort"`
	DevUser           string            `json:"devUser"`
	YardImageRef      YardImageRef      `json:"yardImageRef,omitempty"`
	ResolvedYardImage ResolvedYardImage `json:"resolvedYardImage,omitempty"`
	Projects          []OwnerProject    `json:"projects"`
}

type OwnerInventory struct {
	Schema     int         `json:"schema"`
	HostID     string      `json:"hostId"`
	ObservedAt time.Time   `json:"observedAt"`
	Yards      []OwnerYard `json:"yards"`
}

func (inventory OwnerInventory) Validate() error {
	if inventory.Schema != OwnerInventorySchema {
		return fmt.Errorf("unsupported owner inventory schema %d", inventory.Schema)
	}
	if !SafeID(inventory.HostID) || strings.ContainsAny(inventory.HostID, `/\`) {
		return fmt.Errorf("invalid owner HostID %q", inventory.HostID)
	}
	if inventory.ObservedAt.IsZero() {
		return errors.New("owner inventory observedAt is required")
	}
	if len(inventory.Yards) > 1024 {
		return errors.New("owner inventory has too many yards")
	}
	yards := make(map[string]struct{}, len(inventory.Yards))
	projects := make(map[string]struct{})
	for _, yard := range inventory.Yards {
		if !SafeName(yard.Name) {
			return fmt.Errorf("invalid owner yard name %q", yard.Name)
		}
		if _, duplicate := yards[yard.Name]; duplicate {
			return fmt.Errorf("duplicate owner yard %q", yard.Name)
		}
		yards[yard.Name] = struct{}{}
		if yard.Kind != string(YardContainer) && yard.Kind != string(YardVM) {
			return fmt.Errorf("invalid kind for owner yard %q", yard.Name)
		}
		if !SafeName(yard.Instance) || !SafeName(yard.DevUser) ||
			yard.SSHPort < 1 || yard.SSHPort > 65535 {
			return fmt.Errorf("invalid runtime facts for owner yard %q", yard.Name)
		}
		if len(yard.Projects) > 100000 {
			return fmt.Errorf("owner yard %q has too many projects", yard.Name)
		}
		projectIDs := make(map[string][]string, len(yard.Projects))
		projectNames := make(map[string]string, len(yard.Projects))
		for _, project := range yard.Projects {
			if !SafeID(project.ProjectID) || !SafeProjectName(project.Name) {
				return fmt.Errorf("invalid project identity in owner yard %q", yard.Name)
			}
			if project.IdentityVersion != 0 && project.IdentityVersion != 2 {
				return fmt.Errorf("invalid project identity version in owner yard %q", yard.Name)
			}
			if project.IdentityVersion == 2 && project.ProjectID != project.Name {
				return fmt.Errorf("invalid canonical project identity in owner yard %q", yard.Name)
			}
			if project.SourceKey != "" {
				if len(project.SourceKey) != 64 ||
					strings.Trim(project.SourceKey, "0123456789abcdef") != "" {
					return fmt.Errorf("invalid project source key in owner yard %q", yard.Name)
				}
			}
			key := yard.Name + "\x00" + project.ProjectID
			if _, duplicate := projects[key]; duplicate {
				return fmt.Errorf("duplicate project %q in owner yard %q", project.ProjectID, yard.Name)
			}
			projects[key] = struct{}{}
			idKey := ProjectNameKey(project.ProjectID)
			projectIDs[idKey] = append(projectIDs[idKey], project.ProjectID)
			if project.Mode == "" || strings.ContainsAny(project.Mode, "\r\n\t") ||
				strings.ContainsAny(project.Target, "\r\n\t") {
				return fmt.Errorf("invalid project fields in owner yard %q", yard.Name)
			}
		}
		for _, project := range yard.Projects {
			nameKey := ProjectNameKey(project.Name)
			if owner, duplicate := projectNames[nameKey]; duplicate && owner != project.ProjectID {
				return fmt.Errorf(
					"duplicate project name %q in owner yard %q",
					project.Name, yard.Name,
				)
			}
			projectNames[nameKey] = project.ProjectID
			for _, owner := range projectIDs[nameKey] {
				if owner != project.ProjectID {
					return fmt.Errorf(
						"project name %q shadows project %q in owner yard %q",
						project.Name, owner, yard.Name,
					)
				}
			}
		}
	}
	return nil
}
