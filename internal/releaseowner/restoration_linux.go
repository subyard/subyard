//go:build linux

package releaseowner

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/Subyard/Subyard/internal/ports"
	"golang.org/x/sys/unix"
)

// This fact proves only one local registration rename. It neither closes an
// opaque command nor authorizes a different invocation to retry it.
type ownerRestoration struct {
	Version int                     `json:"version"`
	Binding Binding                 `json:"binding"`
	Token   string                  `json:"token"`
	Recipe  string                  `json:"recipe"`
	Source  string                  `json:"source"`
	Member  ports.NativeOwnerMember `json:"member"`
}

func validOwnerRestorationMember(m ports.NativeOwnerMember) bool {
	mode := m.Mode & 07777
	return m.Device != 0 && m.Inode != 0 && m.UID == uint32(os.Getuid()) && m.Mode&unix.S_IFMT == unix.S_IFREG && (mode == 0600 || mode == 0640 || mode == 0644) && m.Links == 1 && m.Size >= 0 && m.Size <= 32<<20 && m.ModifiedNanos >= 0 && m.ModifiedNanos < 1e9 && m.ChangedNanos >= 0 && m.ChangedNanos < 1e9 && fingerprint(m.Digest)
}

func (o *invocationObserver) restorationIntent(token, source string) (Intent, error) {
	var intent Intent
	if token != o.ownerToken || len(token) != 32 || strings.Trim(token, "0123456789abcdef") != "" || !fingerprint(source) {
		return intent, errors.New("invalid owner restoration scope")
	}
	directory, err := o.session.factsDirectory(o.binding)
	if err != nil {
		return intent, err
	}
	defer unix.Close(directory)
	raw, err := readFactAt(directory, factName(o.binding.Ordinal, "intent"))
	if err != nil {
		return intent, err
	}
	if strict(raw, &intent) != nil || intent.Version != 1 || intent.Binding != o.binding || intent.Owner != "owner-registration" || intent.Resource != "test-yard" || !fingerprint(intent.Recipe) {
		return intent, errors.New("owner restoration intent differs")
	}
	canonical, err := json.Marshal(intent)
	if err != nil || !bytes.Equal(append(canonical, '\n'), raw) {
		return intent, errors.New("noncanonical owner restoration intent")
	}
	var accepted Terminal
	raw, err = readFactAt(directory, factName(o.binding.Ordinal, "accepted"))
	if err != nil || strict(raw, &accepted) != nil || accepted.Version != 1 || accepted.Binding != o.binding || accepted.Owner != intent.Owner || accepted.Resource != intent.Resource || accepted.State != "accepted" {
		return intent, errors.New("owner restoration acceptance differs")
	}
	canonical, err = json.Marshal(accepted)
	if err != nil || !bytes.Equal(append(canonical, '\n'), raw) {
		return intent, errors.New("noncanonical owner restoration acceptance")
	}
	return intent, nil
}

func (o *invocationObserver) RecordOwnerRestoration(token, source string, member ports.NativeOwnerMember) (err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	defer func() {
		if err != nil {
			o.fault = errors.Join(o.fault, err)
		}
	}()
	intent, err := o.restorationIntent(token, source)
	if err != nil {
		return err
	}
	if !validOwnerRestorationMember(member) {
		return errors.New("invalid owner restoration member")
	}
	return o.session.writeFact(o.binding, "restoration", ownerRestoration{1, o.binding, token, intent.Recipe, source, member})
}

func (o *invocationObserver) ReadOwnerRestoration(token, source string) (ports.NativeOwnerMember, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	intent, err := o.restorationIntent(token, source)
	if err != nil {
		return ports.NativeOwnerMember{}, err
	}
	directory, err := o.session.factsDirectory(o.binding)
	if err != nil {
		return ports.NativeOwnerMember{}, err
	}
	defer unix.Close(directory)
	raw, err := readFactAt(directory, factName(o.binding.Ordinal, "restoration"))
	if err != nil {
		return ports.NativeOwnerMember{}, err
	}
	var fact ownerRestoration
	if strict(raw, &fact) != nil || fact.Version != 1 || fact.Binding != o.binding || fact.Token != token || fact.Recipe != intent.Recipe || fact.Source != source || !validOwnerRestorationMember(fact.Member) {
		return ports.NativeOwnerMember{}, errors.New("owner restoration binding differs")
	}
	canonical, err := json.Marshal(fact)
	if err != nil || !bytes.Equal(append(canonical, '\n'), raw) {
		return ports.NativeOwnerMember{}, errors.New("owner restoration encoding differs")
	}
	return fact.Member, nil
}
