// Package yardnetwork owns the host-wide policy for explicit links between yards.
package yardnetwork

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"sort"

	"github.com/Subyard/Subyard/internal/domain"
)

const (
	SchemaVersion = 1
	PolicyKey     = "user.subyard.network_policy"
	MaxPolicySize = 1 << 20
)

type Link struct {
	A string `json:"a"`
	B string `json:"b"`
}

type Policy struct {
	Schema           int       `json:"schema"`
	Isolation        bool      `json:"isolation"`
	Links            []Link    `json:"links,omitempty"`
	Revision         uint64    `json:"revision"`
	AppliedRevision  uint64    `json:"appliedRevision"`
	Bindings         []Binding `json:"bindings,omitempty"`
	AppliedIsolation bool      `json:"appliedIsolation"`
	PendingStart     []string  `json:"pendingStart,omitempty"`
	Removing         []string  `json:"removing,omitempty"`
}

// Binding retains the original settings so disabling isolation restores them.
type Binding struct {
	Yard           Yard              `json:"yard"`
	IPv4           string            `json:"ipv4"`
	MAC            string            `json:"mac"`
	OriginalNIC    map[string]string `json:"originalNic"`
	OriginalAccess string            `json:"originalAccess"`
}

func Decode(content []byte) (Policy, error) {
	if len(content) == 0 {
		return Policy{Schema: SchemaVersion}, nil
	}
	if len(content) > MaxPolicySize {
		return Policy{}, errors.New("network policy exceeds its size bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var policy Policy
	if err := decoder.Decode(&policy); err != nil {
		return Policy{}, fmt.Errorf("decode network policy: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Policy{}, errors.New("network policy has trailing content")
	}
	if err := policy.normalize(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

func Encode(policy Policy) ([]byte, error) {
	policy.Links = slices.Clone(policy.Links)
	policy.Bindings = slices.Clone(policy.Bindings)
	if err := policy.normalize(); err != nil {
		return nil, err
	}
	content, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	if len(content) > MaxPolicySize {
		return nil, errors.New("network policy exceeds its size bound")
	}
	return content, nil
}

func (policy *Policy) Link(a, b string) error {
	link, err := canonicalLink(a, b)
	if err != nil {
		return err
	}
	if !slices.Contains(policy.Links, link) {
		policy.Links = append(policy.Links, link)
		sortLinks(policy.Links)
	}
	return nil
}

func (policy *Policy) Unlink(a, b string) error {
	link, err := canonicalLink(a, b)
	if err != nil {
		return err
	}
	policy.Links = slices.DeleteFunc(policy.Links, func(candidate Link) bool { return candidate == link })
	return nil
}

func (policy *Policy) Forget(name string) {
	policy.Links = slices.DeleteFunc(policy.Links, func(link Link) bool { return link.A == name || link.B == name })
	policy.Bindings = slices.DeleteFunc(policy.Bindings, func(binding Binding) bool { return binding.Yard.Name == name })
}

func (policy Policy) Peers(name string) []string {
	var result []string
	for _, link := range policy.Links {
		switch name {
		case link.A:
			result = append(result, link.B)
		case link.B:
			result = append(result, link.A)
		}
	}
	sort.Strings(result)
	return result
}

func (policy *Policy) normalize() error {
	if policy.Schema != SchemaVersion {
		return fmt.Errorf("unsupported network policy schema %d", policy.Schema)
	}
	if policy.AppliedRevision > policy.Revision {
		return errors.New("network policy applied revision exceeds its desired revision")
	}
	seen := make(map[Link]bool, len(policy.Links))
	for index, candidate := range policy.Links {
		link, err := canonicalLink(candidate.A, candidate.B)
		if err != nil {
			return err
		}
		if seen[link] {
			return errors.New("network policy contains a duplicate link")
		}
		seen[link] = true
		policy.Links[index] = link
	}
	sortLinks(policy.Links)
	bindings := map[string]bool{}
	identities := map[string]bool{}
	addresses := map[string]bool{}
	macs := map[string]bool{}
	for _, binding := range policy.Bindings {
		ip, ipErr := netip.ParseAddr(binding.IPv4)
		mac, macErr := net.ParseMAC(binding.MAC)
		if !domain.SafeName(binding.Yard.Name) || !domain.SafeName(binding.Yard.Project) || !domain.SafeName(binding.Yard.Instance) || !domain.SafeName(binding.Yard.Network) || bindings[binding.Yard.Name] || ipErr != nil || !ip.Is4() || macErr != nil || len(mac) != 6 || mac[0]&1 != 0 || binding.OriginalNIC["type"] != "nic" || binding.OriginalNIC["network"] != binding.Yard.Network {
			return errors.New("network policy contains an invalid or duplicate yard binding")
		}
		bindings[binding.Yard.Name] = true
		identity := binding.Yard.Project + "/" + binding.Yard.Instance
		address := binding.Yard.Network + "/" + binding.IPv4
		macIdentity := binding.Yard.Network + "/" + mac.String()
		if identities[identity] || addresses[address] || macs[macIdentity] {
			return errors.New("network policy contains colliding yard identities")
		}
		identities[identity] = true
		addresses[address] = true
		macs[macIdentity] = true
	}
	for _, names := range [][]string{policy.PendingStart, policy.Removing} {
		seenNames := map[string]bool{}
		for _, name := range names {
			if !domain.SafeName(name) || seenNames[name] {
				return errors.New("network policy contains invalid pending yard names")
			}
			seenNames[name] = true
		}
	}
	for _, name := range policy.Removing {
		if slices.Contains(policy.PendingStart, name) {
			return errors.New("network policy cannot restart a yard being removed")
		}
	}
	sort.Slice(policy.Bindings, func(i, j int) bool { return policy.Bindings[i].Yard.Name < policy.Bindings[j].Yard.Name })
	return nil
}

func canonicalLink(a, b string) (Link, error) {
	if !domain.SafeName(a) || !domain.SafeName(b) || a == b {
		return Link{}, errors.New("a network link requires two distinct local yard names")
	}
	if b < a {
		a, b = b, a
	}
	return Link{A: a, B: b}, nil
}

func sortLinks(links []Link) {
	sort.Slice(links, func(left, right int) bool {
		if links[left].A != links[right].A {
			return links[left].A < links[right].A
		}
		return links[left].B < links[right].B
	})
}
