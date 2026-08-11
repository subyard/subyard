package domain

import (
	"errors"
	"fmt"
	"strings"
)

type YardRef struct {
	HostID   string `json:"hostId"`
	YardName string `json:"yardName"`
}

func (ref YardRef) Validate() error {
	if !SafeID(ref.HostID) || strings.ContainsAny(ref.HostID, `/\`) {
		return fmt.Errorf("invalid owner HostID %q", ref.HostID)
	}
	if !SafeName(ref.YardName) {
		return fmt.Errorf("invalid yard name %q", ref.YardName)
	}
	return nil
}

func (ref YardRef) String() string {
	return ref.HostID + "/" + ref.YardName
}

type YardSelector struct {
	HostID   string
	YardName string
}

func ParseYardSelector(value string) (YardSelector, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return YardSelector{}, errors.New("yard selector is required")
	}
	parts := strings.Split(value, "/")
	switch len(parts) {
	case 1:
		if !SafeName(parts[0]) {
			return YardSelector{}, fmt.Errorf("invalid yard selector %q", value)
		}
		return YardSelector{YardName: parts[0]}, nil
	case 2:
		ref := YardRef{HostID: parts[0], YardName: parts[1]}
		if err := ref.Validate(); err != nil {
			return YardSelector{}, fmt.Errorf("invalid yard selector %q: %w", value, err)
		}
		return YardSelector(ref), nil
	default:
		return YardSelector{}, fmt.Errorf("invalid yard selector %q", value)
	}
}
