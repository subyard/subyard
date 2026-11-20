package reconcileruntime

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"sync/atomic"
)

const MaxInstalledRecipe = 96 << 20

var installedRuntimeHelpers = []string{"download.sh", "engine-context.sh", "host-path-ownership.py", "host.sh", "native-install-custody.py", "native-install-custody.sh", "native-installed-stage.py", "native-installed-stage.sh", "owner-address.sh", "preview-proxy.sh", "runtime.sh", "ssh-config.sh", "ssh-listener.sh", "teardown-files.py", "teardown-plan.py", "teardown-profile-checks.py", "teardown-services.sh", "teardown-session.sh", "teardown-shared.py", "ui.sh"}

type installedRecipeFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}
type installedSourceGuard struct {
	Path   string `json:"path"`
	Before string `json:"before"`
}
type installedRecipe struct {
	Version    int                    `json:"version"`
	Owner      string                 `json:"owner"`
	Source     string                 `json:"source"`
	Runtime    string                 `json:"runtime"`
	Resources  string                 `json:"resources"`
	Launcher   string                 `json:"launcher"`
	HookBefore string                 `json:"hookBefore"`
	Leaves     []string               `json:"leaves"`
	Files      []installedRecipeFile  `json:"files"`
	Profile    string                 `json:"profile,omitempty"`
	Handler    string                 `json:"handler,omitempty"`
	Native     []installedSourceGuard `json:"native,omitempty"`
}

func (p *PreparedInstalledRuntime) Recipe() []byte {
	r := installedRecipe{Version: 1, Owner: p.owner, Source: p.source, Runtime: p.runtime, Resources: p.resources, Launcher: p.keyLauncher, HookBefore: p.hookBefore, Leaves: p.leaves, Profile: p.profile, Handler: p.handler, Native: p.native}
	paths := make([]string, 0, len(p.files))
	for path := range p.files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		r.Files = append(r.Files, installedRecipeFile{path, p.files[path]})
	}
	raw, _ := json.Marshal(r)
	return raw
}

// PreparedInstalledRuntime retains the approved source bytes that the release
// transition reads to render the owner project configuration.
type PreparedInstalledRuntime struct {
	owner, source, runtime string
	leaves                 []string
	files                  map[string][]byte
	keyLauncher            string
	hookBefore, resources  string
	closed                 atomic.Bool
	profile, handler       string
	native                 []installedSourceGuard
	executionFiles         map[string][]byte
}

func (p *PreparedInstalledRuntime) Close() error {
	if p == nil {
		return nil
	}
	p.closed.Store(true)
	return nil
}

// stageUsage charges the exact future tree, including the fixed marker and
// every unique parent directory. Prepare and bind call it before consent or
// effects; the transport's file ceiling alone is not an execution admission.
func (p *PreparedInstalledRuntime) stageUsage() (int, int, error) {
	files := p.files
	if p.executionFiles != nil {
		files = p.executionFiles
	}
	members := map[string]bool{".stage.json": true}
	bytes := 0
	for path, payload := range files {
		if !validMaterializedLayout(path) || path == ".stage.json" {
			return 0, 0, errors.New("invalid installed stage path")
		}
		members[path] = true
		bytes += len(payload)
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			if _, conflict := files[parent]; conflict {
				return 0, 0, errors.New("installed execution stage path collision")
			}
			members[parent] = true
		}
	}
	if len(members) > 8192 || bytes > 64<<20 {
		return len(members), bytes, errors.New("installed execution stage capacity")
	}
	return len(members), bytes, nil
}

func (runtime Runtime) installedResourceBinding() string {
	paths := runtime.Yard.Paths
	paths.RepositoryRoot = runtime.bindingPath(paths.RepositoryRoot)
	paths.ConfigDir = runtime.bindingPath(paths.ConfigDir)
	return installedRuntimeDigest(paths)
}
