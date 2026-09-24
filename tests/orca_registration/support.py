import json
from pathlib import Path
import subprocess
import sys

COMPONENT = Path(__file__).resolve().parents[2] / "config/profiles/orca/resources/orca/registration"
sys.path.insert(0, str(COMPONENT))


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


class Catalog:
    """Only the external RPC boundary is replaced; files, Git and sidecar are real."""

    def __init__(self):
        self.groups = []
        self.repos = []
        self.snapshots = []
        self.folders = []
        self.runtime_id = "runtime-1"
        self.calls = []
        self.after = None

    def call(self, method, params=None, before_send=None):
        if before_send:
            before_send(self.runtime_id)
        self.calls.append((method, params))
        if method == "repo.list":
            result = {"repos": self.repos}
        elif method == "session.tabs.listAll":
            result = {"snapshots": self.snapshots}
        elif method == "folderWorkspace.list":
            result = {"folderWorkspaces": self.folders}
        elif method == "repo.rm":
            self.repos = [r for r in self.repos if "id:" + r["id"] != params["repo"]]
            result = {}
        elif method == "projectGroup.list":
            result = {"groups": self.groups}
        elif method == "projectGroup.delete":
            self.groups = [g for g in self.groups if g["id"] != params["groupId"]]
            result = {}
        elif method == "projectGroup.update":
            group = next(g for g in self.groups if g["id"] == params["groupId"])
            group.update(params["updates"])
            result = {"group": group}
        elif method == "projectGroup.create":
            group = {"id": "group-" + str(len(self.groups) + 1), "name": params["name"],
                     "parentPath": params.get("parentPath"), "createdFrom": params["createdFrom"],
                     "connectionId": None, "parentGroupId": None, "color": None,
                     "tabOrder": len(self.groups), "isCollapsed": False,
                     "createdAt": 1, "updatedAt": 1}
            self.groups.append(group)
            result = {"group": group}
        elif method == "repo.add":
            repo = next((r for r in self.repos if r["path"] == params["path"]), None)
            if repo is None:
                repo = {"id": "repo-" + str(len(self.repos) + 1), "path": params["path"],
                        "kind": params["kind"], "displayName": Path(params["path"]).name, "projectGroupId": None,
                        "projectGroupOrder": 0, "badgeColor": "default", "addedAt": 1,
                        "externalWorktreeVisibility": "hide", "sessionIds": []}
                self.repos.append(repo)
            result = {"repo": repo}
        elif method in ("repo.update", "projectGroup.moveProject"):
            if not params["repo"].startswith("id:"):
                raise AssertionError("repo selectors must use the stable ID")
            repo = next(r for r in self.repos if r["id"] == params["repo"][3:])
            if method == "repo.update":
                repo.update(params["updates"])
            else:
                repo["projectGroupId"] = params["groupId"]
                repo["projectGroupOrder"] = params.get("order", 1000)
            result = {"repo": repo}
        else:
            raise AssertionError("Unexpected RPC mutation: " + method)
        if self.after:
            self.after(method, params)
        return json.loads(json.dumps(result))
