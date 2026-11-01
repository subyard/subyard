//go:build linux

package releaseowner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/ports"
	"golang.org/x/sys/unix"
)

type hostPowerInstallationFact struct {
	Version int                               `json:"version"`
	Binding Binding                           `json:"binding"`
	Result  ports.NativeHostPowerInstallation `json:"result"`
}

func validHostPowerInstallation(v ports.NativeHostPowerInstallation) bool {
	for _, value := range []string{v.Producer, v.Intent, v.Recipe, v.Source, v.HostSource, v.Output, v.Consumer.EngineDigest, v.Consumer.UnitDigest, v.Consumer.ContractsDigest, v.Consumer.FilesDigest} {
		if !fingerprint(value) {
			return false
		}
	}
	if v.Command == 0 || v.Command > 256 || len(v.Identities) != 3 || len(v.Consumer.Generations) != 3 {
		return false
	}
	for i, role := range []string{"engine", "service", "publication"} {
		if v.Identities[i].Role != role || v.Identities[i].Inode == 0 || v.Consumer.Generations[i].Role != role || !fingerprint(v.Consumer.Generations[i].Value) || v.Consumer.Generations[i].Value != v.Consumer.Generations[0].Value {
			return false
		}
	}
	return true
}
func (o *invocationObserver) RecordHostPowerInstallation(command uint64, v ports.NativeHostPowerInstallation) (result ports.NativeHostPowerInstallation, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	defer func() {
		if err != nil {
			o.fault = errors.Join(o.fault, err)
		}
	}()
	if o.restored || o.fault != nil || command == 0 || command > uint64(len(o.commands)) || !o.commandTerminal[command] || o.commands[command-1].Kind != "owner-host-power-install" || v.Producer != "" || v.Intent != "" || v.Recipe != "" || v.Source != "" || v.Command != 0 {
		return result, errors.New("host installation command unavailable")
	}
	root, err := o.creationRoot()
	if err != nil {
		return result, err
	}
	raw, err := json.Marshal(root)
	if err != nil {
		return result, err
	}
	v.Producer = ownerCreateProducer(o.binding)
	v.Intent = digest(raw)
	v.Recipe = root.Recipe
	v.Source = root.Source
	v.Command = command
	if !validHostPowerInstallation(v) {
		return result, errors.New("host installation shape")
	}
	if err = o.session.writeFact(o.binding, "host-power-installation", hostPowerInstallationFact{1, o.binding, v}); err != nil {
		return result, err
	}
	return v, nil
}
func (o *invocationObserver) ReadHostPowerInstallation(producer string) (*ports.NativeHostPowerInstallation, error) {
	if producer != "" && producer != ownerCreateProducer(o.binding) {
		origin := o.session.ownerOrigins[producer]
		if origin == nil || origin == o || !sameOwnerProducerScope(origin.binding, o.binding) {
			return nil, errors.New("host installation producer unavailable")
		}
		current, e := o.creationRoot()
		if e != nil {
			return nil, e
		}
		prior, e := origin.creationRoot()
		if e != nil {
			return nil, e
		}
		if o.action != nil || origin.action != nil {
			if !o.sameOwnerActionProgram(origin) || current.Source != prior.Source {
				return nil, errors.New("host installation program differs")
			}
		} else {
			currentBinding := o.binding
			currentBinding.Ordinal = origin.binding.Ordinal
			current.Binding = prior.Binding
			if currentBinding != origin.binding || current != prior {
				return nil, errors.New("host installation original intent differs")
			}
		}
		return origin.ReadHostPowerInstallation(producer)
	}
	if o.restored {
		terminal, err := o.session.readTerminal(o.binding)
		if err != nil || terminal.Owner != "owner-registration" || terminal.Resource != "test-yard" {
			return nil, errors.New("host installation whole terminal unavailable")
		}
	}
	root, err := o.creationRoot()
	if err != nil {
		return nil, err
	}
	d, err := o.session.factsDirectory(o.binding)
	if err != nil {
		return nil, err
	}
	defer unix.Close(d)
	raw, err := readFactAt(d, factName(o.binding.Ordinal, "host-power-installation"))
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f hostPowerInstallationFact
	if len(raw) > 4096 || strict(raw, &f) != nil {
		return nil, errors.New("host installation fact syntax")
	}
	canonical, _ := json.Marshal(f)
	intent, _ := json.Marshal(root)
	if !bytes.Equal(append(canonical, '\n'), raw) || f.Version != 1 || f.Binding != o.binding || !validHostPowerInstallation(f.Result) || f.Result.Producer != ownerCreateProducer(o.binding) || f.Result.Intent != digest(intent) || f.Result.Recipe != root.Recipe || f.Result.Source != root.Source {
		return nil, errors.New("host installation provenance differs")
	}
	if !hostPowerCommandProven(d, o.binding, f.Result.Command) {
		return nil, errors.New("host installation command proof unavailable")
	}
	copy := f.Result
	return &copy, nil
}
func hostPowerCommandProven(d int, b Binding, index uint64) bool {
	var command commandFact
	completePair := false
	raw, err := readFactAt(d, factName(b.Ordinal, fmtCommand(index, "intent")))
	if err == nil {
		if strict(raw, &command) != nil || command.Kind != "owner-host-power-install" || !fingerprint(command.Source) {
			return false
		}
		canonical, _ := json.Marshal(command)
		if !bytes.Equal(append(canonical, '\n'), raw) {
			return false
		}
		terminal, e := readFactAt(d, factName(b.Ordinal, fmtCommand(index, "terminal")))
		if e == nil {
			if !bytes.Equal(raw, terminal) {
				return false
			}
			completePair = true
		}
		if e != nil && !errors.Is(e, unix.ENOENT) {
			return false
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return false
	}
	raw, err = readFactAt(d, factName(b.Ordinal, "closure"))
	if errors.Is(err, unix.ENOENT) {
		return completePair
	}
	var c ownerCreateClosure
	if err != nil || strict(raw, &c) != nil || c.Version != 1 || c.Binding != b || index > uint64(len(c.Commands)) || c.Commands[index-1].Kind != "owner-host-power-install" || len(c.Commands) > 256 || len(c.Facts) > 1280 || !fingerprint(c.Commands[index-1].Source) {
		return false
	}
	canonical, _ := json.Marshal(c)
	if !bytes.Equal(append(canonical, '\n'), raw) {
		return false
	}
	for _, proof := range []struct{ kind, hash string }{{"intent", c.Intent}, {"accepted", c.Accepted}, {"terminal", c.Terminal}} {
		v, e := readFactAt(d, factName(b.Ordinal, proof.kind))
		if e != nil || digest(v) != proof.hash {
			return false
		}
	}
	encoded, _ := json.Marshal(c.Commands[index-1])
	encoded = append(encoded, '\n')
	for _, phase := range []string{"intent", "terminal"} {
		actual, e := readFactAt(d, factName(b.Ordinal, fmtCommand(index, phase)))
		if errors.Is(e, unix.ENOENT) {
			continue
		}
		if e != nil || !bytes.Equal(actual, encoded) {
			return false
		}
	}
	for _, phase := range []string{"intent", "terminal"} {
		found := false
		for _, f := range c.Facts {
			if f.Name == factName(b.Ordinal, fmtCommand(index, phase)) {
				if found || f.Size != uint64(len(encoded)) || f.SHA256 != digest(encoded) {
					return false
				}
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func fmtCommand(index uint64, phase string) string {
	return fmt.Sprintf("command-%03d-%s", index, phase)
}
