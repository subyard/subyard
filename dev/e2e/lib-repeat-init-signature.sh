#!/usr/bin/env bash
# Identity that a repeated init of a converged yard must leave untouched.
# shellcheck shell=bash

# repeat_init_signature DEFINITION PROJECT INSTANCE
repeat_init_signature() {
  stat -c '%i %a' "$1"
  sha256sum < "$1"
  incus project show "$2"
  incus config show "$3" --project "$2"
  incus list "$3" --project "$2" --format csv -c s
}
