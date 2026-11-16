# shellcheck shell=bash
# Print the bounded tail of captured command output on a lane failure. The fixture uses
# synthetic identities, yet pairing links and long tokens are still redacted.
show_output() {
  local path
  for path; do
    [ -f "$path" ] || continue
    python3 - "$path" <<'PY_LANE_OUTPUT'
import pathlib
import re
import sys

path = pathlib.Path(sys.argv[1])
text = path.read_text(errors="replace")[-16384:]
text = re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", "", text)
text = re.sub(r"orca://\S+", "<redacted>", text)
text = re.sub(r"(?im)^.*(?:authorization|bearer|pairing|token|secret|password).*$", "<redacted>", text)
text = re.sub(r"[A-Za-z0-9_+/=-]{64,}", "<redacted>", text)
for line in text.splitlines()[-40:]:
    print(path.name + ": " + line[:512], file=sys.stderr)
PY_LANE_OUTPUT
  done
}
