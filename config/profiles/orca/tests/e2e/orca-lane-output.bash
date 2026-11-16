# shellcheck shell=bash
# Print the last 40 lines of captured command output on a lane failure. The fixture uses
# synthetic identities, yet pairing links and lines naming credentials are still redacted.
show_output() {
  local path
  for path; do
    [ -f "$path" ] || continue
    python3 - "$path" <<'PY_LANE_OUTPUT'
import pathlib
import re
import sys

path = pathlib.Path(sys.argv[1])
with path.open("rb") as source:
    size = source.seek(0, 2)
    source.seek(max(0, size - 1048576))
    data = source.read()
# Redact whole lines before any truncation, so a cut can never split a secret from its marker.
lines = data.decode(errors="replace").splitlines()
if size > 1048576:
    lines = lines[1:]
redacted = []
for line in lines:
    line = re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", "", line)
    if re.search(r"(?i)authorization|bearer|pairing|token|secret|password|orca://", line):
        line = "<redacted>"
    redacted.append(re.sub(r"[A-Za-z0-9_+/=-]{64,}", "<redacted>", line))
for line in redacted[-40:]:
    print(path.name + ": " + line[:512], file=sys.stderr)
PY_LANE_OUTPUT
  done
}
