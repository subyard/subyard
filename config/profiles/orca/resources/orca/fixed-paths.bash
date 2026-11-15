# shellcheck shell=bash
# Parse the profile's fixed-path list: print "<kind> <path>" per entry. Blank
# lines and lines whose first non-blank character is "#" are skipped; every
# other line is exactly a known kind and one absolute path. The release
# observation provider parses the list by the same rule.
orca_fixed_paths_parse() { # <file>
  local line kind path extra
  while IFS= read -r line || [ -n "$line" ]; do
    read -r kind path extra <<<"$line"
    case "$kind" in ''|'#'*) continue ;; esac
    case "$kind" in unit|unit-helper|path) ;; *) printf 'invalid Orca fixed path list entry\n' >&2; return 1 ;; esac
    [ -z "$extra" ] && [[ "$path" =~ ^/[A-Za-z0-9._/-]+$ ]] && [[ "$path" != *..* ]] || {
      printf 'invalid Orca fixed path list entry\n' >&2; return 1; }
    printf '%s %s\n' "$kind" "$path"
  done <"$1"
}
