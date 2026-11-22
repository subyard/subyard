package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/crypto/ssh"
)

func checkLiveSSH(keyFile io.Reader, user, address string) error {
	keyBytes, err := io.ReadAll(io.LimitReader(keyFile, 16<<10))
	if err != nil {
		return err
	}
	key, err := bareKey(keyBytes)
	if err != nil {
		return errors.New("guest SSH host key could not be verified")
	}
	conn, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		return errors.New("Herdr could not reach the SSH listener")
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	verified := errors.New("live SSH requires a public key")
	policyErr := errors.New("running guest sshd must require public-key authentication only")
	client, _, _, err := ssh.NewClientConn(conn, address, &ssh.ClientConfig{
		User:              user,
		HostKeyCallback:   ssh.FixedHostKey(key),
		HostKeyAlgorithms: []string{key.Type()},
		AuthCallback: func(context *ssh.ClientAuthContext) (ssh.AuthMethod, error) {
			if slices.Equal(context.AllowedMethods, []string{"publickey"}) {
				return nil, verified
			}
			return nil, policyErr
		},
	})
	switch {
	case errors.Is(err, verified):
		return nil
	case err == nil:
		client.Close()
		return policyErr
	case errors.Is(err, policyErr):
		return policyErr
	default:
		return errors.New("could not verify the running SSH listener and guest host key")
	}
}

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
