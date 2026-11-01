package incusclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/lxc/incus/v6/shared/cliconfig"
	"github.com/zitadel/oidc/v3/pkg/oidc"
)

func ownerImageCacheObserver(ctx context.Context) ports.NativeOwnerImageCacheObserver {
	if v := ports.NativeOwnerImageCacheFrom(ctx); v != nil {
		return v
	}
	v, _ := ports.NativeOperationObserverFrom(ctx).(ports.NativeOwnerImageCacheObserver)
	return v
}
func ownerImagePolicyKey(p ports.PreparedOwnerImagePolicy, path []byte) string {
	return ownerCreateDigest(struct {
		Binding string
		Path    []byte
	}{p.CacheBinding, path})
}
func ownerImageCachePolicyShape(p ports.PreparedOwnerImagePolicy) bool {
	if len(p.TokenPaths) == 0 {
		return p.CacheBinding == "" && len(p.Cache) == 0
	}
	if !ownerImageHex(p.CacheBinding, 64) {
		return false
	}
	for _, path := range p.TokenPaths {
		if len(path) == 0 || len(path) > 4096 || !filepath.IsAbs(string(path)) || bytes.IndexByte(path, 0) >= 0 {
			return false
		}
		if !slices.ContainsFunc(p.Guards, func(g ports.OwnerImageSourceGuard) bool {
			return bytes.Equal(g.Path, path) && strings.HasPrefix(g.State, "file:")
		}) {
			return false
		}
	}
	return true
}
func ownerImageCacheAllowedKey(p ports.PreparedOwnerImagePolicy, key string) bool {
	return slices.ContainsFunc(p.TokenPaths, func(path []byte) bool { return ownerImagePolicyKey(p, path) == key })
}

// BindOwnerImagePreparationCache runs once over the existing original cohort
// before its capsule is sealed. It preserves every original before guard.
func (client *Client) BindOwnerImagePreparationCache(ctx context.Context, policies []*ports.PreparedOwnerImagePolicy) error {
	var facts []ports.OwnerImageCacheSuccessor
	for _, p := range policies {
		if p == nil || !ownerImageCachePolicyShape(*p) {
			return domain.ErrPlanStale
		}
		facts = append(facts, p.Cache...)
	}
	for _, p := range policies {
		var joined []ports.OwnerImageCacheSuccessor
		if p.Cache != nil {
			joined = make([]ports.OwnerImageCacheSuccessor, 0)
		}
		for _, fact := range facts {
			if ownerImageCacheAllowedKey(*p, fact.Policy) {
				if err := ownerImageCacheTransition(*p, fact); err != nil {
					return err
				}
				if !slices.ContainsFunc(joined, func(v ports.OwnerImageCacheSuccessor) bool { return reflect.DeepEqual(v, fact) }) {
					joined = append(joined, fact)
				}
			}
		}
		if p.Cache != nil || len(joined) > 0 {
			p.Cache = joined
		}
		guards, err := ownerImageCacheGuards(ctx, *p)
		if err != nil {
			return err
		}
		if err := ownerImageCheckGuardStates(guards); err != nil {
			return err
		}
	}
	return nil
}
func ownerImageCacheGuards(ctx context.Context, p ports.PreparedOwnerImagePolicy) ([]ports.OwnerImageSourceGuard, error) {
	guards := slices.Clone(p.Guards)
	prep := slices.Clone(p.Cache)
	facts := slices.Clone(prep)
	if o := ownerImageCacheObserver(ctx); o != nil {
		for _, path := range p.TokenPaths {
			more, err := o.ReadOwnerImageCache(ownerImagePolicyKey(p, path))
			if err != nil {
				return nil, err
			}
			facts = append(facts, more...)
		}
	}
	if leaf := sourceLeaf(ctx); leaf != nil {
		facts = append(facts, leaf.caches...)
	}
	pending := make([]ports.OwnerImageCacheSuccessor, 0, len(facts))
	for _, fact := range facts {
		if err := ownerImageCacheTransition(p, fact); err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(pending, func(v ports.OwnerImageCacheSuccessor) bool { return reflect.DeepEqual(v, fact) }) {
			pending = append(pending, fact)
		}
	}
	// A branch of the same original path is never an ordering choice.
	for i, a := range pending {
		if reflect.DeepEqual(a.Before, a.After) {
			continue
		}
		for _, b := range pending[i+1:] {
			if reflect.DeepEqual(b.Before, b.After) {
				continue
			}
			for x, before := range a.Before {
				for y, other := range b.Before {
					if bytes.Equal(before.Path, other.Path) && before.State == other.State && a.After[x].State != b.After[y].State {
						return nil, domain.ErrPlanStale
					}
				}
			}
		}
	}
	matches := func(current []ports.OwnerImageSourceGuard, want []ports.OwnerImageSourceGuard) bool {
		for _, g := range want {
			if !slices.ContainsFunc(current, func(v ports.OwnerImageSourceGuard) bool { return reflect.DeepEqual(v, g) }) {
				return false
			}
		}
		return true
	}
	apply := func(current []ports.OwnerImageSourceGuard, after []ports.OwnerImageSourceGuard) {
		for _, g := range after {
			index := slices.IndexFunc(current, func(v ports.OwnerImageSourceGuard) bool { return bytes.Equal(v.Path, g.Path) })
			current[index] = g
		}
	}
	// Only authenticated preparation facts may precede this variant's captured
	// original before. Track those states too so later edges cannot form cycles.
	prior := slices.Clone(guards)
	visited := map[string]bool{ownerCreateDigest(guards): true}
	for {
		selected := -1
		for i, f := range pending {
			if !slices.ContainsFunc(prep, func(v ports.OwnerImageCacheSuccessor) bool { return reflect.DeepEqual(v, f) }) || !matches(prior, f.After) {
				continue
			}
			if reflect.DeepEqual(f.Before, f.After) {
				pending = slices.Delete(pending, i, i+1)
				selected = -2
				break
			}
			if selected >= 0 {
				for _, g := range f.After {
					if slices.ContainsFunc(pending[selected].After, func(v ports.OwnerImageSourceGuard) bool { return bytes.Equal(v.Path, g.Path) }) {
						return nil, domain.ErrPlanStale
					}
				}
				continue
			}
			selected = i
		}
		if selected == -2 {
			continue
		}
		if selected < 0 {
			break
		}
		f := pending[selected]
		apply(prior, f.Before)
		state := ownerCreateDigest(prior)
		if visited[state] {
			return nil, domain.ErrPlanStale
		}
		visited[state] = true
		pending = slices.Delete(pending, selected, selected+1)
	}
	for len(pending) > 0 {
		selected := -1
		for i, f := range pending {
			if !matches(guards, f.Before) {
				continue
			}
			if reflect.DeepEqual(f.Before, f.After) {
				pending = slices.Delete(pending, i, i+1)
				selected = -2
				break
			}
			if selected >= 0 {
				for _, g := range f.Before {
					if slices.ContainsFunc(pending[selected].Before, func(v ports.OwnerImageSourceGuard) bool { return bytes.Equal(v.Path, g.Path) }) {
						return nil, domain.ErrPlanStale
					}
				}
				continue
			}
			selected = i
		}
		if selected == -2 {
			continue
		}
		if selected < 0 {
			return nil, domain.ErrPlanStale
		}
		f := pending[selected]
		apply(guards, f.After)
		state := ownerCreateDigest(guards)
		if visited[state] {
			return nil, domain.ErrPlanStale
		}
		visited[state] = true
		pending = slices.Delete(pending, selected, selected+1)
	}
	return guards, nil
}
func ownerImageCacheTransition(p ports.PreparedOwnerImagePolicy, f ports.OwnerImageCacheSuccessor) error {
	if !ownerImageCachePolicyShape(p) || len(f.Before) == 0 || len(f.Before) > 2 || len(f.Before) != len(f.After) {
		return domain.ErrPlanStale
	}
	token := ""
	for i, b := range f.Before {
		a := f.After[i]
		if !bytes.Equal(b.Path, a.Path) {
			return domain.ErrPlanStale
		}
		path := string(b.Path)
		if strings.HasPrefix(b.State, "file:") && strings.HasPrefix(a.State, "file:") && slices.ContainsFunc(p.TokenPaths, func(t []byte) bool { return string(t) == path }) {
			if token != "" {
				return domain.ErrPlanStale
			}
			token = path
		} else if b.State != "directory:absent" || !strings.HasPrefix(a.State, "directory:") {
			return domain.ErrPlanStale
		}
	}
	if token == "" || f.Policy != ownerImagePolicyKey(p, []byte(token)) {
		return domain.ErrPlanStale
	}
	for _, b := range f.Before {
		if string(b.Path) != token && string(b.Path) != filepath.Dir(token) {
			return domain.ErrPlanStale
		}
	}
	return nil
}
func (client *Client) acceptOwnerImageCache(ctx context.Context, p ports.PreparedOwnerImagePolicy, f ports.OwnerImageCacheSuccessor) error {
	if err := ownerImageCacheTransition(p, f); err != nil {
		return err
	}
	guards, err := ownerImageCacheGuards(ctx, p)
	if err != nil {
		return err
	}
	for i, b := range f.Before {
		index := slices.IndexFunc(guards, func(g ports.OwnerImageSourceGuard) bool { return reflect.DeepEqual(g, b) })
		if index < 0 {
			return domain.ErrPlanStale
		}
		a := f.After[i]
		state, err := ownerImagePathState(string(a.Path), strings.HasPrefix(a.State, "file:"))
		prefix, _, _ := strings.Cut(a.State, ":")
		if err != nil || a.State != prefix+":"+state {
			return domain.ErrPlanStale
		}
	}
	for _, after := range f.After {
		if strings.HasPrefix(after.State, "directory:") {
			info, err := os.Lstat(string(after.Path))
			if err != nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
				return domain.ErrPlanStale
			}
		}
	}
	if err := ownerImageCacheFile(string(f.After[0].Path)); err != nil {
		return err
	}
	if o := ownerImageCacheObserver(ctx); o != nil {
		return o.RecordOwnerImageCache(f)
	}
	// Preparation loses all authority if the complete return capsule is lost.
	if leaf := sourceLeaf(ctx); leaf != nil && leaf.preparation {
		return nil
	}
	return errors.New("cache durable acknowledgement unavailable")
}
func ownerImageCacheFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || s.Uid != uint32(os.Getuid()) || s.Nlink != 1 {
		return domain.ErrPlanStale
	}
	return nil
}

// The SDK silently discards Save errors. Compare its exact serialized token
// pointer and guarded before/after states before publishing any successor.
func (client *Client) saveOwnerImageCache(ctx context.Context, p ports.PreparedOwnerImagePolicy, config *cliconfig.Config, name string, server any) error {
	if !slices.ContainsFunc(p.TokenPaths, func(path []byte) bool { return string(path) == config.OIDCTokenPath(name) }) {
		return domain.ErrPlanStale
	}
	tokens, ok := server.(interface {
		GetOIDCTokens() *oidc.Tokens[*oidc.IDTokenClaims]
	})
	if !ok || tokens.GetOIDCTokens() == nil {
		return domain.ErrPlanStale
	}
	expected, err := json.Marshal(tokens.GetOIDCTokens())
	if err != nil || len(expected) > ownerImageLimit {
		return errors.New("cache serialized token bound")
	}
	path := config.OIDCTokenPath(name)
	originalInfo, originalStatErr := os.Lstat(path)
	guards, err := ownerImageCacheGuards(ctx, p)
	if err != nil {
		return err
	}
	before := []ports.OwnerImageSourceGuard{}
	for _, name := range []string{path, filepath.Dir(path)} {
		index := slices.IndexFunc(guards, func(g ports.OwnerImageSourceGuard) bool { return string(g.Path) == name })
		if index < 0 {
			return domain.ErrPlanStale
		}
		g := guards[index]
		if name != path && g.State != "directory:absent" {
			continue
		}
		before = append(before, g)
	}
	if err := ownerImageCheckGuardsCurrent(before); err != nil {
		return err
	}
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, expected) {
		return ownerImageCacheFile(path)
	}
	if _, err := os.Lstat(path); err == nil {
		if err := ownerImageCacheFile(path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Match the pinned SDK token serialization/path and in-place Save semantics,
	// but retain errors and verify the actual bytes instead of discarding them.
	if len(before) > 1 {
		if err := os.Mkdir(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
			return err
		}
	}
	flags := syscall.O_WRONLY | syscall.O_TRUNC | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
	if before[0].State == "file:absent" {
		flags |= syscall.O_CREAT | syscall.O_EXCL
	}
	fd, err := syscall.Open(path, flags, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	if before[0].State == "file:absent" {
		err = file.Chmod(0o600)
	}
	if err == nil {
		_, err = file.Write(expected)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err := ownerImageCacheFile(path); err != nil {
		return err
	}
	if originalStatErr == nil {
		afterInfo, err := os.Lstat(path)
		if err != nil || !os.SameFile(originalInfo, afterInfo) || originalInfo.Mode() != afterInfo.Mode() {
			return domain.ErrPlanStale
		}
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, expected) {
		return errors.New("SDK cache save unproved")
	}
	after := make([]ports.OwnerImageSourceGuard, len(before))
	for i, b := range before {
		prefix, _, _ := strings.Cut(b.State, ":")
		state, err := ownerImagePathState(string(b.Path), prefix == "file")
		if err != nil {
			return err
		}
		after[i] = ports.OwnerImageSourceGuard{Path: bytes.Clone(b.Path), State: prefix + ":" + state}
	}
	fact := ports.OwnerImageCacheSuccessor{Policy: ownerImagePolicyKey(p, []byte(path)), Before: before, After: after}
	if leaf := sourceLeaf(ctx); leaf != nil {
		if leaf.preparation {
			if err := client.acceptOwnerImageCache(ports.WithNativeOwnerImageCache(ctx, nil), p, fact); err != nil {
				return err
			}
		} else if err := (&ownerSourceObserver{leaf.wire}).RecordOwnerImageCache(fact); err != nil {
			return err
		}
		leaf.caches = append(leaf.caches, fact)
		return nil
	}
	return client.acceptOwnerImageCache(ctx, p, fact)
}
func ownerImageCheckGuardsCurrent(guards []ports.OwnerImageSourceGuard) error {
	for _, g := range guards {
		prefix, state, _ := strings.Cut(g.State, ":")
		actual, err := ownerImagePathState(string(g.Path), prefix == "file")
		if err != nil || state != actual {
			return domain.ErrPlanStale
		}
	}
	return nil
}
