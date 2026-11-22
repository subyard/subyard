package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

func checkSSHPolicy(input io.Reader, user string) error {
	scan := bufio.NewScanner(input)
	accounts := make(map[string]map[string]string)
	var settings map[string]string
	for scan.Scan() {
		key, value, found := strings.Cut(scan.Text(), " ")
		if !found {
			continue
		}
		if key == "context" {
			settings = make(map[string]string)
			accounts[value] = settings
		} else if settings != nil {
			settings[key] = value
		}
	}
	var refusals []error
	if accounts["global"] == nil || accounts[user] == nil {
		refusals = append(refusals, errors.New("could not verify effective guest sshd authentication"))
	}
	for name, settings := range accounts {
		methods := settings["authenticationmethods"]
		if methods != "publickey" {
			if methods != "any" {
				refusals = append(refusals, fmt.Errorf("guest sshd authentication is not key-only for %s", name))
			}
			for _, option := range []string{"passwordauthentication", "kbdinteractiveauthentication", "hostbasedauthentication", "gssapiauthentication", "permitemptypasswords"} {
				if settings[option] != "no" {
					refusals = append(refusals, fmt.Errorf("guest sshd must disable %s for %s before publishing Herdr", option, name))
				}
			}
		}
		if name == user && (settings["pubkeyauthentication"] != "yes" ||
			!slices.Contains(strings.Fields(settings["authorizedkeysfile"]), ".ssh/authorized_keys") &&
				!slices.Contains(strings.Fields(settings["authorizedkeysfile"]), "%h/.ssh/authorized_keys")) {
			refusals = append(refusals, fmt.Errorf("guest sshd must allow public keys from .ssh/authorized_keys for %s", user))
		}
	}
	return domain.CollectRefusals(append(refusals, scan.Err())...)
}
