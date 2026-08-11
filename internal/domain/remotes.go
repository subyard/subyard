package domain

import (
	"encoding/json"
	"errors"
	"time"
)

type RemoteAction string

const (
	RemoteAdd       RemoteAction = "add"
	RemoteRepairKey RemoteAction = "repair-key"
	RemoteRemove    RemoteAction = "remove"
	RemoteList      RemoteAction = "list"
)

type RemoteSpec struct {
	LegacyAlias   string `json:"legacyAlias"`
	OwnerEndpoint string `json:"ownerEndpoint"`
	OwnerYardName string `json:"ownerYardName"`
}

func (spec *RemoteSpec) UnmarshalJSON(payload []byte) error {
	type canonical RemoteSpec
	var value canonical
	if err := json.Unmarshal(payload, &value); err != nil {
		return err
	}
	var legacy struct {
		LegacyAlias   *string `json:"name"`
		OwnerEndpoint *string `json:"destination"`
		OwnerYardName *string `json:"ownerYard"`
	}
	if err := json.Unmarshal(payload, &legacy); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return err
	}
	merge := func(canonicalName string, current *string, legacyName string, old *string) error {
		if old == nil {
			return nil
		}
		if _, present := fields[canonicalName]; present && *current != *old {
			return errors.New("remote spec contains conflicting " + canonicalName + " and legacy " + legacyName)
		}
		if _, present := fields[canonicalName]; !present {
			*current = *old
		}
		return nil
	}
	if err := merge("legacyAlias", &value.LegacyAlias, "name", legacy.LegacyAlias); err != nil {
		return err
	}
	if err := merge("ownerEndpoint", &value.OwnerEndpoint, "destination", legacy.OwnerEndpoint); err != nil {
		return err
	}
	if err := merge("ownerYardName", &value.OwnerYardName, "ownerYard", legacy.OwnerYardName); err != nil {
		return err
	}
	*spec = RemoteSpec(value)
	return nil
}

type RemoteInfo struct {
	YardName         string `json:"yardName"`
	AccessKind       string `json:"accessKind"`
	Version          string `json:"version"`
	YardInstanceName string `json:"yardInstanceName"`
	IncusProject     string `json:"incusProject"`
	State            string `json:"state"`
	SSHHost          string `json:"sshHost"`
	SSHPort          int    `json:"sshPort"`
	DevUser          string `json:"devUser"`
	Projects         *int   `json:"projects"`
}

func (info *RemoteInfo) UnmarshalJSON(payload []byte) error {
	type canonical RemoteInfo
	var value canonical
	if err := json.Unmarshal(payload, &value); err != nil {
		return err
	}
	var legacy struct {
		YardName         *string `json:"name"`
		AccessKind       *string `json:"type"`
		YardInstanceName *string `json:"instance"`
		IncusProject     *string `json:"project"`
	}
	if err := json.Unmarshal(payload, &legacy); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return err
	}
	for _, item := range []struct {
		canonical, legacy string
		current           *string
		old               *string
	}{
		{"yardName", "name", &value.YardName, legacy.YardName},
		{"accessKind", "type", &value.AccessKind, legacy.AccessKind},
		{"yardInstanceName", "instance", &value.YardInstanceName, legacy.YardInstanceName},
		{"incusProject", "project", &value.IncusProject, legacy.IncusProject},
	} {
		if item.old == nil {
			continue
		}
		if _, present := fields[item.canonical]; present && *item.current != *item.old {
			return errors.New("remote info contains conflicting " + item.canonical + " and legacy " + item.legacy)
		}
		if _, present := fields[item.canonical]; !present {
			*item.current = *item.old
		}
	}
	*info = RemoteInfo(value)
	return nil
}

type RemoteKey struct {
	Material    string `json:"material"`
	Fingerprint string `json:"fingerprint"`
}

type RemoteRecord struct {
	Spec      RemoteSpec `json:"spec"`
	Remote    bool       `json:"remote"`
	Path      string     `json:"path,omitempty"`
	SSHPort   int        `json:"sshPort"`
	LastProbe time.Time  `json:"lastProbe,omitempty"`
}

type RemotePrepared struct {
	Action   RemoteAction   `json:"action"`
	Spec     RemoteSpec     `json:"spec"`
	Existing *RemoteRecord  `json:"existing,omitempty"`
	Owner    RemoteInfo     `json:"owner,omitempty"`
	Recorded []RemoteKey    `json:"recorded,omitempty"`
	Scanned  []RemoteKey    `json:"scanned,omitempty"`
	Records  []RemoteRecord `json:"records,omitempty"`
}

type RemoteResult struct {
	Message string         `json:"message,omitempty"`
	Records []RemoteRecord `json:"records,omitempty"`
}

func RemoteKeysOverlap(left, right []RemoteKey) bool {
	for _, first := range left {
		for _, second := range right {
			if first.Material == second.Material {
				return true
			}
		}
	}
	return false
}
