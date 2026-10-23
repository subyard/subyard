package config

import (
	"bytes"
	"os"
	"path/filepath"

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
	// Current provision hooks consume the shared static-delivery helper. Bind it
	// with the resolved package before assessment; older source roots omit it.
	helper := filepath.Join(pkg.Root, "..", "..", "skills", "deliver.py")
	if _, err := os.Lstat(helper); err == nil {
		if err := binding.AddSource("agent-skills.py", helper); err != nil {
			return profile.ProvisionBinding{}, err
		}
	} else if !os.IsNotExist(err) {
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
