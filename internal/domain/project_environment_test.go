package domain

import "testing"

func TestProjectCacheBoundary(t *testing.T) {
	for _, scenario := range []struct {
		path string
		ok   bool
	}{
		{"/srv/cache/npm", true}, {"/srv/cache/pnpm/linux-x86_64/v11", true},
		{"/srv/cache", false}, {"/srv/cache/../env-secrets", false},
		{"relative", false}, {"/srv/cache/npm:/var/run", false},
		{"/srv/cache/node_modules", false}, {"/srv/cache/project/.venv", false},
		{"/srv/cache/docker", false}, {"/srv/cache/.codex/sessions", false},
		{"/var/run/docker.sock", false}, {"/srv/env-secrets/project", false},
		{"/srv/cache/credentials", false}, {"/srv/cache/npm\n", false},
	} {
		t.Run(scenario.path, func(t *testing.T) {
			if err := ValidateProjectCachePath(scenario.path); (err == nil) != scenario.ok {
				t.Fatalf("cache accepted=%t, want %t: %v", err == nil, scenario.ok, err)
			}
		})
	}
}

func TestProjectDependencyMountBoundary(t *testing.T) {
	for _, scenario := range []struct {
		mount string
		ok    bool
	}{
		{"/srv/cache/sdk:/srv/cache/sdk:ro", true},
		{"/opt/jdk-17:/opt/jdk-17:ro", true},
		{"/usr/local/bin/cache-helper:/usr/local/bin/cache-helper:ro", true},
		{"/srv/cache/.cache.lock:/srv/cache/.cache.lock:ro", true},
		{"/srv/cache/sdk:/srv/cache/sdk", false},
		{"/srv/cache/sdk:/srv/cache/sdk:rw", false},
		{"/srv/cache/sdk:/workspace:ro", false},
		{"/srv/cache/sdk:/run/subyard:ro", false},
		{"/srv/cache/sdk:/:ro", false},
		{"/srv/cache/sdk:/home/dev/.venv:ro", false},
		{"/srv/env-secrets/other:/opt/inputs:ro", false},
		{"/srv/workspaces/other/src:/opt/other:ro", false},
		{"/var/run/docker.sock:/var/run/docker.sock:ro", false},
		{"/opt/../root/.codex:/opt/config:ro", false},
		{"/opt/tool:/proc:ro", false},
	} {
		t.Run(scenario.mount, func(t *testing.T) {
			if err := ValidateProjectEnvironmentMount(scenario.mount); (err == nil) != scenario.ok {
				t.Fatalf("mount accepted=%t, want %t: %v", err == nil, scenario.ok, err)
			}
		})
	}
}
