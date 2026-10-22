package config

import (
	"bytes"

	"github.com/Subyard/Subyard/internal/profile"
)

// ProfileValues uses the existing assignment evaluator on a bound shipped file.
func ProfileValues(source *profile.Source, base map[string]string) (map[string]string, error) {
	if source == nil {
		return nil, nil
	}
	data, err := source.Read()
	if err != nil {
		return nil, err
	}
	return EvaluateAssignments(bytes.NewReader(data), source.Path, base)
}

func ProfileProvisionBinding(pkg profile.Package, base map[string]string) (profile.ProvisionBinding, error) {
	binding, err := pkg.BindProvision()
	if err != nil {
		return profile.ProvisionBinding{}, err
	}
	declared, err := ProfileValues(pkg.Config, nil)
	if err != nil {
		return profile.ProvisionBinding{}, err
	}
	values, err := ProfileValues(pkg.Config, base)
	if err != nil {
		return profile.ProvisionBinding{}, err
	}
	binding.Environment = make(map[string]string, len(declared))
	for name := range declared {
		binding.Environment[name] = values[name]
	}
	return binding, binding.Check()
}
