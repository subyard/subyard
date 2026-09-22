"""Discover the mandatory project folder and every nested Git working-tree root.

Traversal uses open directory descriptors and O_NOFOLLOW, including for project
metadata. Git runs in that held directory; directory symlinks and .git contents
are never traversed. Ignore files and repository nesting do not prune the scan.
"""

from dataclasses import dataclass, field
import json
import os
import stat
import subprocess
import time


@dataclass
class Root:
    path: str
    name: str
    kind: str


@dataclass
class Project:
    project_id: str
    name: str
    root: str
    roots: list = field(default_factory=list)


@dataclass
class Scan:
    projects: list = field(default_factory=list)
    errors: list = field(default_factory=list)
    warnings: list = field(default_factory=list)
    workspaces: str = ""
    identity: tuple = ()


class ScanLimit(Exception):
    pass


def _open_dir(path, parent=None):
    return os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)


def _same_directory(path, fd):
    try:
        expected = os.stat(path, follow_symlinks=False)
        actual = os.fstat(fd)
        return (stat.S_ISDIR(expected.st_mode) and os.path.realpath(path) == path
                and (expected.st_dev, expected.st_ino) == (actual.st_dev, actual.st_ino))
    except OSError:
        return False


def _git_kind(path, fd, marker, deadline):
    if not _same_directory(path, fd):
        return "unknown"
    environment = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
    environment.update({"LC_ALL": "C", "GIT_OPTIONAL_LOCKS": "0", "GIT_TERMINAL_PROMPT": "0"})
    try:
        remaining = min(3.0, deadline - time.monotonic())
        if remaining <= 0:
            raise ScanLimit()
        probe = subprocess.run(["git", "rev-parse", "--show-toplevel"],
                               cwd="/proc/self/fd/" + str(fd), pass_fds=(fd,),
                               env=environment, capture_output=True, timeout=remaining)
        if not _same_directory(path, fd):
            return "unknown"
        if probe.returncode == 0:
            toplevel = os.fsdecode(probe.stdout[:-1] if probe.stdout.endswith(b"\n") else probe.stdout)
            return "git" if toplevel == path else "folder"
        if not marker and probe.stderr.startswith(b"fatal: not a git repository"):
            return "folder"
    except (OSError, subprocess.TimeoutExpired):
        pass
    return "unknown"


def verify_root(root, deadline):
    """Recheck the filesystem fact immediately before applying its metadata."""
    try:
        if os.path.realpath(root.path) != root.path:
            return False
        fd = _open_dir(root.path)
        try:
            try:
                os.stat(".git", dir_fd=fd, follow_symlinks=False)
                marker = True
            except FileNotFoundError:
                marker = False
            return _git_kind(root.path, fd, marker, deadline) == root.kind
        finally:
            os.close(fd)
    except (OSError, ScanLimit):
        return False


def verify_missing(scan, path):
    """Require ENOENT below the same workspace boundary; never follow symlinks."""
    if not path.startswith(scan.workspaces + "/") or os.path.normpath(path) != path:
        return False
    fd = None
    try:
        fd = _open_dir(scan.workspaces)
        current = os.fstat(fd)
        if ((current.st_dev, current.st_ino) != scan.identity
                or not _same_directory(scan.workspaces, fd)):
            return False
        ancestor = scan.workspaces
        for name in os.path.relpath(path, scan.workspaces).split("/"):
            try:
                child = _open_dir(name, fd)
            except FileNotFoundError:
                return _same_directory(ancestor, fd)
            os.close(fd)
            fd = child
            ancestor = os.path.join(ancestor, name)
    except OSError:
        pass
    finally:
        if fd is not None:
            os.close(fd)
    return False


def discover(workspaces, deadline=None, max_entries=100000):
    workspaces = os.path.abspath(workspaces)
    scan = Scan(workspaces=workspaces)
    deadline = deadline if deadline is not None else time.monotonic() + 20.0
    entries_left = max_entries

    def check():
        nonlocal entries_left
        entries_left -= 1
        if entries_left < 0 or time.monotonic() >= deadline:
            raise ScanLimit()

    def entries(fd):
        found = []
        with os.scandir(fd) as iterator:
            for item in iterator:
                check()
                found.append((item.name, item.is_dir(follow_symlinks=False)))
        return sorted(found)

    def walk(project, root_fd):
        # Iterators hold at most one descriptor per depth, without Python recursion.
        stack = [(project.root, root_fd, None)]
        try:
            while stack:
                path, fd, children = stack[-1]
                if children is None:
                    check()
                    if not _same_directory(path, fd):
                        raise OSError("directory changed")
                    try:
                        os.stat(".git", dir_fd=fd, follow_symlinks=False)
                        marker = True
                    except FileNotFoundError:
                        marker = False
                    except OSError:
                        scan.errors.append("Directory scan failed: " + path)
                        os.close(fd)
                        stack.pop()
                        continue
                    if path == project.root or marker:
                        kind = _git_kind(path, fd, marker, deadline)
                        name = project.name if path == project.root else os.path.relpath(path, project.root)
                        if path == project.root or kind in ("git", "unknown"):
                            project.roots.append(Root(path, name, kind))
                        if kind == "unknown":
                            scan.errors.append("Git root could not be verified: " + path)
                    try:
                        children = iter(entries(fd))
                    except OSError:
                        scan.errors.append("Directory scan failed: " + path)
                        children = iter(())
                    stack[-1] = (path, fd, children)
                try:
                    name, is_directory = next(children)
                except StopIteration:
                    os.close(fd)
                    stack.pop()
                    continue
                if name == ".git" or not is_directory:
                    continue
                child = os.path.join(path, name)
                try:
                    child_fd = _open_dir(name, fd)
                except OSError:
                    scan.errors.append("Directory scan failed: " + child)
                    continue
                stack.append((child, child_fd, None))
        except OSError:
            scan.errors.append("Workspace changed during discovery: " + project.root)
        finally:
            for _, fd, _ in stack:
                os.close(fd)

    try:
        check()
        if os.path.realpath(workspaces) != workspaces:
            raise OSError("workspace boundary is a symlink")
        base_fd = _open_dir(workspaces)
        identity = os.fstat(base_fd)
        scan.identity = (identity.st_dev, identity.st_ino)
        try:
            for project_id, is_directory in entries(base_fd):
                if not is_directory:
                    continue
                check()
                try:
                    project_fd = _open_dir(project_id, base_fd)
                except OSError:
                    scan.errors.append("Project directory is inaccessible: " + project_id)
                    continue
                try:
                    try:
                        metadata_fd = os.open(".subyard-meta.json", os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                                              dir_fd=project_fd)
                    except FileNotFoundError:
                        continue
                    try:
                        with os.fdopen(metadata_fd, "rb") as source:
                            if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
                                raise ValueError()
                            raw = source.read(65537)
                        metadata = json.loads(raw) if len(raw) <= 65536 else None
                        if (not isinstance(metadata, dict) or type(metadata.get("schema")) is not int
                                or metadata["schema"] != 1 or metadata.get("projectId") != project_id
                                or not isinstance(metadata.get("name"), str) or not metadata["name"].strip()):
                            raise ValueError()
                    except (OSError, ValueError, RecursionError):
                        scan.errors.append("Invalid project metadata: " + project_id)
                        continue
                    root = os.path.join(workspaces, project_id, "src")
                    project = Project(project_id, metadata["name"], root)
                    scan.projects.append(project)
                    try:
                        root_fd = _open_dir("src", project_fd)
                    except FileNotFoundError:
                        scan.warnings.append("Project root is missing; existing Orca records are retained: " + root)
                        continue
                    except OSError:
                        scan.errors.append("Project root is inaccessible or a symlink: " + root)
                        continue
                    walk(project, root_fd)
                    project.roots.sort(key=lambda item: (item.path != root, item.path))
                except OSError:
                    scan.errors.append("Project scan failed: " + project_id)
                finally:
                    os.close(project_fd)
        finally:
            os.close(base_fd)
    except ScanLimit:
        scan.errors.append("Workspace discovery limit reached; expected repository set is incomplete")
    except OSError:
        scan.errors.append("Workspace directory is unavailable or changed during discovery")
    return scan
