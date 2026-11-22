#!/bin/sh
# Validate sshd syntax and the caller's policy before reloading the guest service.
reload_guest_sshd() {
    /usr/sbin/sshd -t && "$@" && systemctl reload ssh.service
}
