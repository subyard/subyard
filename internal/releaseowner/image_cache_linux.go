package releaseowner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/ports"
	"golang.org/x/sys/unix"
)

type ownerImageCacheFact struct {
	Version        int
	Binding        Binding
	Recipe, Source string
	Successor      ports.OwnerImageCacheSuccessor
}

func validImageCache(v ports.OwnerImageCacheSuccessor) bool {
	if !fingerprint(v.Policy) || len(v.Before) == 0 || len(v.Before) > 4 || len(v.Before) != len(v.After) {
		return false
	}
	for i, b := range v.Before {
		a := v.After[i]
		if !bytes.Equal(b.Path, a.Path) || !filepath.IsAbs(string(b.Path)) || len(b.Path) > 4096 {
			return false
		}
		for _, g := range []ports.OwnerImageSourceGuard{a, b} {
			prefix, state, ok := strings.Cut(g.State, ":")
			if !ok || (prefix != "file" && prefix != "directory") || state != "absent" && !fingerprint(state) {
				return false
			}
		}
	}
	return true
}
func (o *invocationObserver) imageCacheRoot() (Intent, error) {
	var intent Intent
	d, err := o.session.factsDirectory(o.binding)
	if err != nil {
		return intent, err
	}
	defer unix.Close(d)
	raw, err := readFactAt(d, factName(o.binding.Ordinal, "intent"))
	if err != nil {
		return intent, err
	}
	if strict(raw, &intent) != nil || intent.Version != 1 || intent.Binding != o.binding || intent.Owner != "owner-registration" || intent.Resource != "test-yard" || !fingerprint(intent.Recipe) || !fingerprint(intent.Source) {
		return intent, errors.New("cache original invocation differs")
	}
	return intent, nil
}
func (o *invocationObserver) RecordOwnerImageCache(v ports.OwnerImageCacheSuccessor) (err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	defer func() {
		if err != nil {
			o.fault = errors.Join(o.fault, err)
		}
	}()
	if o.fault != nil || o.restored || o.cacheCount >= 64 || !validImageCache(v) {
		return errors.New("cache successor admission")
	}
	root, err := o.imageCacheRoot()
	if err != nil {
		return err
	}
	d, err := o.session.factsDirectory(o.binding)
	if err != nil {
		return err
	}
	defer unix.Close(d)
	for i := 1; i <= 64; i++ {
		raw, readErr := readFactAt(d, factName(o.binding.Ordinal, fmt.Sprintf("image-cache-%03d", i)))
		if errors.Is(readErr, unix.ENOENT) {
			break
		}
		var prior ownerImageCacheFact
		if readErr != nil || len(raw) > 64<<10 || strict(raw, &prior) != nil || prior.Version != 1 || prior.Binding != o.binding || prior.Recipe != root.Recipe || prior.Source != root.Source || !validImageCache(prior.Successor) {
			return errors.New("cache predecessor unavailable")
		}
		canonical, _ := json.Marshal(prior)
		if !bytes.Equal(append(canonical, '\n'), raw) {
			return errors.New("cache predecessor noncanonical")
		}
		o.cacheCount = i
	}
	if o.cacheCount >= 64 {
		return errors.New("cache successor bound")
	}
	o.cacheCount++
	return o.session.writeFact(o.binding, fmt.Sprintf("image-cache-%03d", o.cacheCount), ownerImageCacheFact{1, o.binding, root.Recipe, root.Source, v})
}

// Read is endpoint provenance only. It deliberately accepts the exact pending
// invocation without manufacturing an outer terminal or replay grant.
func (o *invocationObserver) readOwnerImageCacheExact(key string) ([]ports.OwnerImageCacheSuccessor, error) {
	if !fingerprint(key) {
		return nil, errors.New("cache policy binding")
	}
	root, err := o.imageCacheRoot()
	if err != nil {
		return nil, err
	}
	d, err := o.session.factsDirectory(o.binding)
	if err != nil {
		return nil, err
	}
	defer unix.Close(d)
	var result []ports.OwnerImageCacheSuccessor
	for i := 1; i <= 64; i++ {
		raw, err := readFactAt(d, factName(o.binding.Ordinal, fmt.Sprintf("image-cache-%03d", i)))
		if errors.Is(err, unix.ENOENT) {
			break
		}
		if err != nil || len(raw) > 64<<10 {
			return nil, errors.New("cache fact unavailable")
		}
		var fact ownerImageCacheFact
		if strict(raw, &fact) != nil {
			return nil, errors.New("cache fact syntax")
		}
		canonical, _ := json.Marshal(fact)
		if !bytes.Equal(append(canonical, '\n'), raw) || fact.Version != 1 || fact.Binding != o.binding || fact.Recipe != root.Recipe || fact.Source != root.Source || !validImageCache(fact.Successor) {
			return nil, errors.New("cache fact binding")
		}
		if fact.Successor.Policy == key {
			result = append(result, fact.Successor)
		}
	}
	return result, nil
}

func (o *invocationObserver) ReadOwnerImageCache(key string) ([]ports.OwnerImageCacheSuccessor, error) {
	result, err := o.readOwnerImageCacheExact(key)
	if err != nil {
		return nil, err
	}
	root, err := o.imageCacheRoot()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(o.session.ownerOrigins))
	for k := range o.session.ownerOrigins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		prior := o.session.ownerOrigins[k]
		if prior == o || prior.binding == o.binding {
			continue
		}
		if !sameOwnerProducerScope(prior.binding, o.binding) {
			continue
		}
		before, err := prior.imageCacheRoot()
		if err != nil || before.Source != root.Source || !(o.action == nil && prior.action == nil && before.Recipe == root.Recipe || o.sameOwnerActionProgram(prior)) {
			return nil, errors.New("cache producer scope differs")
		}
		facts, err := prior.readOwnerImageCacheExact(key)
		if err != nil {
			return nil, err
		}
		result = append(facts, result...)
	}
	return result, nil
}
