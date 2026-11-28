"""Fixtures for the shared checkout discovery library."""

import json
from pathlib import Path
import subprocess
import sys

LIBRARY = Path(__file__).resolve().parents[2] / "scripts/lib/checkout-discovery"
sys.path.insert(0, str(LIBRARY))


def project(workspaces, project_id="sample-id", name="Sample"):
    directory = Path(workspaces) / project_id
    directory.mkdir(parents=True)
    (directory / ".subyard-meta.json").write_text(json.dumps({
        "schema": 1, "projectId": project_id, "name": name,
    }))
    root = directory / "src"
    root.mkdir()
    return root


def git(directory, *args):
    return subprocess.run(["git", "-C", str(directory), *args], check=True,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True).stdout


def init_git(directory, commit=False):
    Path(directory).mkdir(parents=True, exist_ok=True)
    git(directory, "init", "-q")
    if commit:
        git(directory, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
            "commit", "-q", "--allow-empty", "-m", "Fixture")
