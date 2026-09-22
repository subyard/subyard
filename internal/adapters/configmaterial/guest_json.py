import fcntl
import hashlib
import json
import os
import re
import secrets
import stat
import sys
from decimal import Decimal
from datetime import date, datetime, time

STATE_ROOT = "@STATE_ROOT@"
STATE_UID = @STATE_UID@
FORMAT = "@FORMAT@"

if FORMAT == "toml":
    import tomllib


class MaterializationError(Exception):
    pass


def fail(category):
    print("config materialization: " + category, file=sys.stderr)
    raise SystemExit(2)


def pointer_part(value):
    return value.replace("~", "~0").replace("/", "~1")


def pointer_parts(pointer):
    if not pointer.startswith("/"):
        raise MaterializationError("invalid materialization baseline")
    result = []
    for encoded in pointer[1:].split("/"):
        decoded = encoded.replace("~1", "/").replace("~0", "~")
        if pointer_part(decoded) != encoded:
            raise MaterializationError("invalid materialization baseline")
        result.append(decoded)
    return result


def ownership(value, prefix=""):
    result = []
    if isinstance(value, dict):
        if not value and prefix:
            result.append({"path": prefix, "kind": "empty-object"})
        for key in sorted(value):
            path = prefix + "/" + pointer_part(key)
            child = value[key]
            if isinstance(child, dict):
                result.extend(ownership(child, path))
            else:
                result.append({"path": path, "kind": "value"})
    return result


def object_paths(value, prefix=""):
    result = set()
    if isinstance(value, dict):
        if prefix:
            result.add(prefix)
        for key, child in value.items():
            if isinstance(child, dict):
                result.update(object_paths(child, prefix + "/" + pointer_part(key)))
    return result


def strict_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate key")
        result[key] = value
    return result


def reject_constant(_value):
    raise ValueError("non-finite number")


def parse_json(payload):
    return json.loads(payload, object_pairs_hook=strict_object,
                      parse_int=Decimal, parse_float=Decimal,
                      parse_constant=reject_constant)


def parse_document(payload):
    if FORMAT == "json":
        return parse_json(payload)
    if isinstance(payload, bytes):
        payload = payload.decode("utf-8")
    return tomllib.loads(payload, parse_float=Decimal)


def lookup(root, parts):
    value = root
    for part in parts:
        if not isinstance(value, dict) or part not in value:
            return False, None
        value = value[part]
    return True, value


def json_equal(left, right):
    if isinstance(left, Decimal) or isinstance(right, Decimal):
        if (FORMAT == "toml" and isinstance(left, Decimal) and
                isinstance(right, Decimal) and left.is_nan() and right.is_nan()):
            return True
        return isinstance(left, Decimal) and isinstance(right, Decimal) and left == right
    if isinstance(left, bool) or isinstance(right, bool):
        return isinstance(left, bool) and isinstance(right, bool) and left == right
    if left is None or right is None:
        return left is None and right is None
    if isinstance(left, str) or isinstance(right, str):
        return isinstance(left, str) and isinstance(right, str) and left == right
    if isinstance(left, list) or isinstance(right, list):
        return (isinstance(left, list) and isinstance(right, list) and
                len(left) == len(right) and
                all(json_equal(a, b) for a, b in zip(left, right)))
    if isinstance(left, dict) or isinstance(right, dict):
        return (isinstance(left, dict) and isinstance(right, dict) and
                set(left) == set(right) and
                all(json_equal(left[key], right[key]) for key in left))
    if FORMAT == "toml" and type(left) is type(right) and isinstance(left, (int, date, time)):
        return left == right
    return False


def remove_owned(root, entry):
    parts = pointer_parts(entry["path"])
    if not parts:
        return
    parent = root
    parents = []
    for part in parts[:-1]:
        if not isinstance(parent, dict) or part not in parent:
            return
        parents.append((parent, part))
        parent = parent[part]
    if not isinstance(parent, dict) or parts[-1] not in parent:
        return
    current = parent[parts[-1]]
    if entry["kind"] == "value" or (isinstance(current, dict) and not current):
        del parent[parts[-1]]
        for ancestor, key in reversed(parents):
            child = ancestor.get(key)
            if isinstance(child, dict) and not child:
                del ancestor[key]
            else:
                break


def overlay(current, desired):
    for key, wanted in desired.items():
        if isinstance(wanted, dict):
            existing = current.get(key)
            if not isinstance(existing, dict):
                existing = {}
                current[key] = existing
            overlay(existing, wanted)
        else:
            current[key] = wanted


def validate_baseline(payload, developer, destination):
    try:
        value = parse_json(payload)
    except Exception:
        raise MaterializationError("invalid materialization baseline")
    if not isinstance(value, dict):
        raise MaterializationError("invalid materialization baseline")
    fields = {"schema", "developer", "destination", "desired_digest", "owned"}
    if value.get("schema") == 2:
        fields.update({"format", "phase", "projection_digest", "retired_digest"})
    if set(value) != fields:
        raise MaterializationError("invalid materialization baseline")
    if (not isinstance(value["schema"], Decimal) or value["schema"] not in (1, 2) or
            not isinstance(value["developer"], str) or value["developer"] != developer or
            not isinstance(value["destination"], str) or value["destination"] != destination):
        raise MaterializationError("invalid materialization baseline")
    if not isinstance(value["desired_digest"], str) or not re.fullmatch(r"[0-9a-f]{64}", value["desired_digest"]):
        raise MaterializationError("invalid materialization baseline")
    if value["schema"] == 2:
        if (value["format"] != FORMAT or value["phase"] not in ("applied", "retiring", "retired") or
                not isinstance(value["projection_digest"], str) or
                not re.fullmatch(r"[0-9a-f]{64}", value["projection_digest"]) or
                not isinstance(value["retired_digest"], str) or
                (value["phase"] == "applied" and value["retired_digest"] != "") or
                (value["phase"] != "applied" and
                 not re.fullmatch(r"[0-9a-f]{64}", value["retired_digest"]))):
            raise MaterializationError("invalid materialization baseline")
    owned = value["owned"]
    if not isinstance(owned, list):
        raise MaterializationError("invalid materialization baseline")
    previous = ""
    paths = []
    for entry in owned:
        if not isinstance(entry, dict) or set(entry) != {"path", "kind"}:
            raise MaterializationError("invalid materialization baseline")
        if entry["kind"] not in ("value", "empty-object") or not isinstance(entry["path"], str):
            raise MaterializationError("invalid materialization baseline")
        parts = pointer_parts(entry["path"])
        if any(parts[:len(other)] == other or other[:len(parts)] == parts for other in paths):
            raise MaterializationError("invalid materialization baseline")
        paths.append(parts)
        encoded = entry["path"] + "\0" + entry["kind"]
        if encoded <= previous:
            raise MaterializationError("invalid materialization baseline")
        previous = encoded
    return value


def validate_regular(path, owner, exact_mode=None):
    info = os.lstat(path)
    if not stat.S_ISREG(info.st_mode) or info.st_uid != owner or info.st_nlink != 1:
        raise MaterializationError("invalid materialization state")
    if exact_mode is not None and stat.S_IMODE(info.st_mode) != exact_mode:
        raise MaterializationError("invalid materialization state")


def compatible_baseline(evidence, released=False):
    # This exact schema is shared with retained runtimes. Extra ownership proof
    # lives beside it, under the same protected directory and per-document lock.
    return {"schema": 1, "developer": evidence["developer"],
            "destination": evidence["destination"],
            "desired_digest": evidence["desired_digest"],
            "owned": [] if released else evidence["owned"]}


def read_ownership(receipt_path, baseline, developer, destination):
    receipt = None
    if os.path.lexists(receipt_path):
        validate_regular(receipt_path, STATE_UID, 0o600)
        with open(receipt_path, "rb") as source:
            receipt = validate_baseline(source.read(), developer, destination)
        if receipt["schema"] != 2:
            raise MaterializationError("invalid ownership receipt")
    if baseline is not None and baseline["schema"] == 2:
        if receipt is None or receipt["phase"] != "retiring":
            return baseline
        # An older candidate may have been interrupted after publishing the
        # companion intent but before converting its shared schema2 baseline.
        baseline = compatible_baseline(baseline, baseline["phase"] == "retired")
    if receipt is None:
        return None
    matching = (receipt["phase"] != "retired" and
                baseline == compatible_baseline(receipt))
    released = (receipt["phase"] != "applied" and
                baseline == compatible_baseline(receipt, True))
    if matching or released:
        return receipt
    if receipt["phase"] == "retiring":
        raise MaterializationError("pending retirement baseline changed")
    # A retained writer may have replaced its schema1 baseline. Its metadata is
    # still usable for normal materialization, but no longer proves retirement.
    return None


def validate_state_root():
    if not os.path.lexists(STATE_ROOT):
        return False
    info = os.lstat(STATE_ROOT)
    if (not stat.S_ISDIR(info.st_mode) or info.st_uid != STATE_UID or
            stat.S_IMODE(info.st_mode) & 0o077):
        raise MaterializationError("invalid materialization state")
    return True


def read_current(destination):
    if not os.path.lexists(destination):
        return {}, None
    try:
        info = os.lstat(destination)
        if not stat.S_ISREG(info.st_mode):
            raise MaterializationError("invalid destination path")
        with open(destination, "rb") as source:
            raw = source.read()
        value = parse_document(raw)
        if not isinstance(value, dict):
            raise ValueError()
        return value, raw
    except MaterializationError:
        raise
    except Exception:
        raise MaterializationError("invalid current " + FORMAT.upper())


def validate_destination_parent(destination, allowed_home, uid, create):
    directory = os.path.dirname(destination)
    if destination == allowed_home or not destination.startswith(allowed_home + os.sep):
        raise MaterializationError("invalid destination path")
    flags = os.O_RDONLY | os.O_DIRECTORY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = None
    try:
        descriptor = os.open(allowed_home, flags)
        relative = os.path.relpath(directory, allowed_home)
        for part in [] if relative == "." else relative.split(os.sep):
            try:
                child = os.open(part, flags, dir_fd=descriptor)
            except FileNotFoundError:
                if not create:
                    return False
                os.mkdir(part, 0o755, dir_fd=descriptor)
                child = os.open(part, flags, dir_fd=descriptor)
                os.fchown(child, uid, uid)
                os.fchmod(child, 0o755)
            os.close(descriptor)
            descriptor = child
    except Exception:
        raise MaterializationError("invalid destination path")
    finally:
        if descriptor is not None:
            os.close(descriptor)
    return True


def semantic_payload(value):
    if isinstance(value, dict):
        return ("{" + ",".join(
            json.dumps(key, ensure_ascii=False, separators=(",", ":")) + ":" +
            semantic_payload(value[key]).decode()
            for key in sorted(value)) + "}").encode()
    if isinstance(value, list):
        return ("[" + ",".join(semantic_payload(item).decode() for item in value) + "]").encode()
    if isinstance(value, Decimal):
        if not value.is_finite():
            raise MaterializationError("invalid JSON number")
        if value == 0:
            return b"0"
        normalized = format(value, "f")
        if "." in normalized:
            normalized = normalized.rstrip("0").rstrip(".")
        return normalized.encode()
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()


def fingerprint(current, desired, baseline_digest, state, tracked):
    entries = {(entry["path"], entry["kind"]): entry for entry in ownership(desired)}
    for entry in tracked:
        entries[(entry["path"], entry["kind"])] = entry
    materialized = {"schema": 1, "state": state, "desired_digest": baseline_digest,
                    "projection": projection(current, entries.values())}
    payload = (toml_fingerprint_payload(materialized) if FORMAT == "toml"
               else semantic_payload(materialized))
    return hashlib.sha256(payload).hexdigest()


def projection(current, entries):
    result = []
    for entry in sorted(entries, key=lambda item: (item["path"], item["kind"])):
        found, value = lookup(current, pointer_parts(entry["path"]))
        if entry["kind"] == "empty-object":
            value = isinstance(value, dict) if found else None
        result.append({"path": entry["path"], "kind": entry["kind"],
                       "found": found, "value": value if found else None})
    return result


def projection_digest(current, entries):
    value = projection(current, entries)
    payload = toml_fingerprint_payload(value) if FORMAT == "toml" else semantic_payload(value)
    return hashlib.sha256(payload).hexdigest()


def legacy_json_matches(current, baseline):
    # Schema1 recorded the old template's semantic digest and owned pointers.
    # Reconstruct only those values; unrelated current fields are not evidence.
    previous = {}
    for entry in baseline["owned"]:
        parts = pointer_parts(entry["path"])
        found, value = lookup(current, parts)
        if not found:
            return False
        if entry["kind"] == "empty-object":
            if not isinstance(value, dict):
                return False
            value = {}
        elif isinstance(value, dict):
            return False
        parent = previous
        for part in parts[:-1]:
            parent = parent.setdefault(part, {})
        parent[parts[-1]] = value
    # Match Go's JSON string encoding used by the historical desired digest.
    payload = semantic_payload(previous).replace(b"\xe2\x80\xa8", b"\\u2028").replace(b"\xe2\x80\xa9", b"\\u2029")
    return hashlib.sha256(payload).hexdigest() == baseline["desired_digest"]


def toml_fingerprint_payload(value):
    # Preserve TOML scalar types (including dates and non-finite floats) without
    # persisting values in the ownership baseline or exposing them in diagnostics.
    def typed(current):
        if isinstance(current, dict):
            return ["table", [[key, typed(current[key])] for key in sorted(current)]]
        if isinstance(current, list):
            return ["array", [typed(item) for item in current]]
        if isinstance(current, Decimal):
            if current.is_nan():
                encoded = "nan"
            elif current == 0:
                encoded = "0"
            else:
                encoded = format(current, "f")
                if "." in encoded:
                    encoded = encoded.rstrip("0").rstrip(".")
            return ["float", encoded]
        if isinstance(current, (datetime, date, time)):
            return [type(current).__name__, current.isoformat()]
        return [type(current).__name__, current]
    return json.dumps(typed(value), ensure_ascii=False, separators=(",", ":")).encode()


def atomic_write(path, payload, uid, mode):
    directory = os.path.dirname(path)
    directory_flags = os.O_RDONLY | os.O_DIRECTORY
    if hasattr(os, "O_NOFOLLOW"):
        directory_flags |= os.O_NOFOLLOW
    directory_descriptor = os.open(directory, directory_flags)
    temporary = ".subyard-config." + secrets.token_hex(12)
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(temporary, flags, 0o600, dir_fd=directory_descriptor)
    try:
        with os.fdopen(descriptor, "wb") as output:
            output.write(payload)
            output.flush()
            os.fchown(output.fileno(), uid, uid)
            os.fchmod(output.fileno(), mode)
            os.fsync(output.fileno())
        os.replace(temporary, os.path.basename(path),
                   src_dir_fd=directory_descriptor, dst_dir_fd=directory_descriptor)
        os.fsync(directory_descriptor)
        temporary = None
    finally:
        if temporary is not None:
            try:
                os.unlink(temporary, dir_fd=directory_descriptor)
            except FileNotFoundError:
                pass
        os.close(directory_descriptor)


def replace_destination(destination, allowed_home, uid, original, payload, create, file_mode=0o644):
    validate_destination_parent(destination, allowed_home, uid, create)
    _, latest = read_current(destination)
    if latest != original:
        raise MaterializationError("concurrent destination change")
    atomic_write(destination, payload, uid, file_mode)
    _, latest = read_current(destination)
    if latest != payload:
        raise MaterializationError("destination verification failed")


def document_payload(current, original):
    if original is not None and json_equal(current, parse_document(original)):
        return original
    if FORMAT == "toml":
        return _toml_writer["dumps"](current).encode()
    return semantic_payload(current) + b"\n"


def retire(mode, baseline, baseline_path, receipt_path, current, original, destination, allowed_home, uid):
    if baseline is None or baseline["schema"] != 2:
        raise MaterializationError("retirement ownership evidence unavailable")
    if original is None:
        raise MaterializationError("retirement destination missing")
    phase = baseline["phase"]
    actual = projection_digest(current, baseline["owned"])
    if phase != "retired":
        expected = {baseline["projection_digest"]}
        if phase == "retiring":
            expected.add(baseline["retired_digest"])
        if actual not in expected:
            raise MaterializationError("retirement owned fields changed")
    if mode == "retire" and phase != "retired":
        if phase == "applied" or actual == baseline["projection_digest"]:
            for entry in sorted(baseline["owned"], key=lambda item: item["path"].count("/"), reverse=True):
                remove_owned(current, entry)
            # Commit intent before replacing the document. A retry accepts only
            # the recorded before/after managed projections, never arbitrary drift.
            baseline["phase"] = "retiring"
            baseline["retired_digest"] = projection_digest(current, baseline["owned"])
            atomic_write(receipt_path, semantic_payload(baseline) + b"\n", STATE_UID, 0o600)
            # Convert an earlier candidate's schema2 before any document edit.
            atomic_write(baseline_path, semantic_payload(compatible_baseline(baseline)) + b"\n", STATE_UID, 0o600)
            replace_destination(destination, allowed_home, uid, original,
                                document_payload(current, original), False,
                                stat.S_IMODE(os.lstat(destination).st_mode))
        atomic_write(baseline_path, semantic_payload(compatible_baseline(baseline, True)) + b"\n", STATE_UID, 0o600)
        baseline["phase"] = "retired"
        atomic_write(receipt_path, semantic_payload(baseline) + b"\n", STATE_UID, 0o600)
        phase = "retired"
    report = {"converged": phase == "retired",
              "fingerprint": fingerprint(current, {}, baseline["projection_digest"], phase,
                                         [] if phase == "retired" else baseline["owned"])}
    print(json.dumps(report, separators=(",", ":"), sort_keys=True))


def main():
    if len(sys.argv) != 7:
        fail("invalid request")
    mode, developer, destination, uid_text, desired_digest, allowed_home = sys.argv[1:]
    if mode not in ("observe", "apply", "assess-adopt", "assess-retire", "retire"):
        fail("invalid request")
    read_only = mode in ("observe", "assess-adopt", "assess-retire")
    try:
        uid = int(uid_text)
    except ValueError:
        fail("invalid request")
    try:
        desired = parse_document(sys.stdin.read())
    except Exception:
        fail("invalid desired " + FORMAT.upper())
    if not isinstance(desired, dict):
        fail("desired configuration root must be an object")
    wanted_owned = sorted(ownership(desired), key=lambda item: (item["path"], item["kind"]))
    identity = hashlib.sha256((developer + "\0" + destination).encode()).hexdigest()
    baseline_path = os.path.join(STATE_ROOT, identity + ".json")
    receipt_path = os.path.join(STATE_ROOT, identity + ".ownership")
    lock_path = os.path.join(STATE_ROOT, identity + ".lock")
    state_exists = validate_state_root()

    if mode in ("observe", "assess-adopt") and not os.path.lexists(baseline_path):
        parent_exists = validate_destination_parent(destination, allowed_home, uid, False)
        current, original = read_current(destination) if parent_exists else ({}, None)
        if mode == "assess-adopt" and original is not None:
            raise MaterializationError("adoption ownership evidence unavailable")
        report = {"converged": False,
                  "fingerprint": fingerprint(current, desired, "missing", "baseline-missing", [])}
        print(json.dumps(report, separators=(",", ":"), sort_keys=True))
        return

    if mode in ("assess-retire", "retire") and not os.path.lexists(baseline_path):
        raise MaterializationError("retirement ownership evidence unavailable")

    if mode == "apply":
        if not state_exists:
            os.makedirs(STATE_ROOT, mode=0o700)
            os.chmod(STATE_ROOT, 0o700)
        lock_flags = os.O_RDWR | os.O_CREAT
        if hasattr(os, "O_NOFOLLOW"):
            lock_flags |= os.O_NOFOLLOW
        lock_descriptor = os.open(lock_path, lock_flags, 0o600)
        lock_info = os.fstat(lock_descriptor)
        if not stat.S_ISREG(lock_info.st_mode) or lock_info.st_uid != STATE_UID:
            os.close(lock_descriptor)
            raise MaterializationError("invalid materialization state")
        os.fchmod(lock_descriptor, 0o600)
    else:
        if not os.path.lexists(lock_path):
            raise MaterializationError("invalid materialization baseline")
        validate_regular(lock_path, STATE_UID, 0o600)
        lock_flags = os.O_RDONLY
        if hasattr(os, "O_NOFOLLOW"):
            lock_flags |= os.O_NOFOLLOW
        lock_descriptor = os.open(lock_path, lock_flags)

    with os.fdopen(lock_descriptor, "rb" if mode != "apply" else "r+b") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_SH if read_only else fcntl.LOCK_EX)
        baseline = None
        if os.path.lexists(baseline_path):
            validate_regular(baseline_path, STATE_UID, 0o600)
            with open(baseline_path, "rb") as source:
                baseline = validate_baseline(source.read(), developer, destination)
        evidence = read_ownership(receipt_path, baseline, developer, destination)
        compatible = baseline is not None and baseline["schema"] == 1
        parent_exists = validate_destination_parent(destination, allowed_home, uid, False)
        current, original = read_current(destination) if parent_exists else ({}, None)

        if mode in ("assess-retire", "retire"):
            retire(mode, evidence, baseline_path, receipt_path, current, original, destination, allowed_home, uid)
            return

        if evidence is not None:
            baseline = evidence

        if mode in ("observe", "assess-adopt"):
            baseline_current = (baseline["desired_digest"] == desired_digest and
                                baseline["owned"] == wanted_owned and
                                baseline.get("phase", "applied") == "applied")
            managed_current = original is not None
            for entry in wanted_owned:
                found_current, current_value = lookup(current, pointer_parts(entry["path"]))
                found_desired, desired_value = lookup(desired, pointer_parts(entry["path"]))
                if not found_current or not found_desired:
                    managed_current = False
                elif entry["kind"] == "empty-object":
                    managed_current = managed_current and isinstance(current_value, dict)
                else:
                    managed_current = managed_current and json_equal(current_value, desired_value)
            converged = baseline_current and managed_current
            if mode == "observe" and (evidence is None or not compatible):
                converged = False
            adoptable = False
            bound_digest = baseline["desired_digest"]
            if mode == "assess-adopt":
                if baseline.get("phase") == "retiring":
                    raise MaterializationError("materialization retirement pending")
                if original is not None:
                    if baseline["schema"] == 2:
                        adoptable = (baseline["phase"] == "applied" and
                                     projection_digest(current, baseline["owned"]) == baseline["projection_digest"])
                    elif FORMAT == "json":
                        adoptable = legacy_json_matches(current, baseline)
                    else:
                        adoptable = converged
                    if not adoptable:
                        raise MaterializationError("adoption owned fields or evidence changed")
                bound_digest += ":" + desired_digest
            report = {"converged": converged,
                      "fingerprint": fingerprint(current, desired, bound_digest,
                                                 "converged" if converged else "drift",
                                                 baseline["owned"])}
            if mode == "assess-adopt":
                report["adoptable"] = adoptable
            print(json.dumps(report, separators=(",", ":"), sort_keys=True))
            return

        if baseline is not None and baseline.get("phase") == "retiring":
            raise MaterializationError("materialization retirement pending")
        if baseline is not None and baseline.get("phase") != "retired":
            wanted_keys = {(entry["path"], entry["kind"]) for entry in wanted_owned}
            wanted_objects = object_paths(desired)
            retired = [entry for entry in baseline["owned"]
                       if (entry["path"], entry["kind"]) not in wanted_keys and
                       entry["path"] not in wanted_objects]
            for entry in sorted(retired, key=lambda item: item["path"].count("/"), reverse=True):
                remove_owned(current, entry)
        overlay(current, desired)
        replace_destination(destination, allowed_home, uid, original,
                            document_payload(current, original), True)
        evidence = {"schema": 2, "developer": developer,
                    "destination": destination, "desired_digest": desired_digest,
                    "format": FORMAT, "phase": "applied",
                    "projection_digest": projection_digest(current, wanted_owned),
                    "retired_digest": "", "owned": wanted_owned}
        atomic_write(baseline_path, semantic_payload(compatible_baseline(evidence)) + b"\n", STATE_UID, 0o600)
        atomic_write(receipt_path, semantic_payload(evidence) + b"\n", STATE_UID, 0o600)


try:
    main()
except MaterializationError as error:
    fail(str(error))
except Exception:
    fail("operation failed")
