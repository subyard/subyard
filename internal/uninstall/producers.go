package uninstall

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"golang.org/x/sys/unix"
)

// ProducerEvidence binds historical producer templates and helper hashes.
type ProducerEvidence struct {
	EngineSHA256          string
	ExecutingEngineSHA256 string
	Templates             map[string]string
}

// Fixture seams: the system owner UID and the relay unit directory.
var (
	producerSystemUID  uint32
	relayUnitDirectory = "/etc/systemd/system"
)

// CaptureProducers shares the exact owner proof with ordinary lifecycle removal.
func CaptureProducers(root string, plan RecoveryPlan, environment map[string]string, owners []string, port int, original ProducerEvidence) ([]RecoveryService, error) {
	services, err := coreProducers(root, plan.Installation, environment, owners, plan.Yards, port, original)
	if err != nil {
		return nil, classify("capture-producers", err)
	}
	return services, nil
}

type producerFile struct {
	path     string
	expected []byte
	helper   bool
	owned    bool
}

var publicationSuffix = regexp.MustCompile(`^\n\[Service\]\nEnvironment=SUBYARD_PUBLICATION_CONTRACTS=[A-Za-z0-9+/=]+\n$`)

func producerTemplateMatches(content, expected []byte, publication bool) bool {
	return bytes.Equal(content, expected) || publication && bytes.HasPrefix(content, expected) && publicationSuffix.Match(content[len(expected):])
}

func captureService(scope string, units []string, files []producerFile, uid uint32) (RecoveryService, error) {
	artifacts := []ports.TeardownArtifact{}
	add := func(path string, check func(int, string, []Entry) error) error {
		return withParent(path, "producer artifact changed", func(parent int, name string) error {
			facts, err := walkEntries(parent, name, artifactLimit, nil, nil, false, nil)
			if err != nil || len(facts) == 0 {
				return err
			}
			if err := check(parent, name, facts); err != nil {
				return err
			}
			binding, err := digest(facts)
			artifacts = append(artifacts, ports.TeardownArtifact{Path: path, Entries: facts, Binding: binding})
			return err
		})
	}
	helpers := len(slices.DeleteFunc(slices.Clone(files), func(file producerFile) bool { return !file.helper })) > 0
	for _, file := range files {
		err := add(file.path, func(parent int, name string, facts []Entry) error {
			mode := uint32(0644)
			if file.helper {
				mode = 0755
			}
			if fact := facts[0]; fact.Mode&unix.S_IFMT != unix.S_IFREG || fact.UID != uid || fact.Mode&07777 != mode {
				return guardRefusal("unsafe producer artifact")
			}
			if file.helper {
				if !file.owned {
					return guardRefusal("unknown producer helper")
				}
				return nil
			}
			fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			source := os.NewFile(uintptr(fd), name)
			defer source.Close()
			content, err := io.ReadAll(io.LimitReader(source, 1<<20+1))
			if err == nil && !producerTemplateMatches(content, file.expected, helpers) {
				err = guardRefusal("changed or foreign producer template")
			}
			return err
		})
		if err != nil {
			return RecoveryService{}, err
		}
	}
	for _, file := range files {
		if file.helper {
			continue
		}
		for _, target := range []string{"timers.target.wants", "sockets.target.wants", "multi-user.target.wants"} {
			link := filepath.Join(filepath.Dir(file.path), target, filepath.Base(file.path))
			err := withParent(link, "producer enablement changed", func(parent int, name string) error {
				facts, err := walkEntries(parent, name, artifactLimit, nil, nil, false, nil)
				if err != nil {
					return err
				}
				buffer := make([]byte, unix.PathMax+1)
				size, err := unix.Readlinkat(parent, name, buffer)
				if err != nil {
					return err
				}
				destination := string(buffer[:size])
				if !filepath.IsAbs(destination) {
					destination = filepath.Join(filepath.Dir(link), destination)
				}
				if filepath.Clean(destination) != file.path {
					return guardRefusal("foreign producer enablement")
				}
				binding, err := digest(facts)
				artifacts = append(artifacts, ports.TeardownArtifact{Path: link, Entries: facts, Binding: binding})
				return err
			})
			if err != nil {
				return RecoveryService{}, err
			}
		}
	}
	slices.SortStableFunc(artifacts, func(left, right ports.TeardownArtifact) int { return strings.Compare(left.Path, right.Path) })
	return RecoveryService{Scope: scope, Units: units, Artifacts: artifacts}, nil
}

func lexists(path string) bool { _, err := os.Lstat(path); return err == nil }

func regularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func fileDigest(path string) ([sha256.Size]byte, error) {
	content, err := os.ReadFile(path)
	return sha256.Sum256(content), err
}

func coreProducers(root string, installation Installation, environment map[string]string, owners []string, yards []RecoveryYard, port int, original ProducerEvidence) ([]RecoveryService, error) {
	result := []RecoveryService{}
	add := func(scope string, units []string, files []producerFile, uid uint32) error {
		service, err := captureService(scope, units, files, uid)
		if err == nil && len(service.Artifacts) > 0 {
			result = append(result, service)
		}
		return err
	}
	template := func(name string) ([]byte, error) {
		return readBounded(filepath.Join(root, "config/systemd", name), 1<<20)
	}
	value := func(name, fallback string) string {
		if environment[name] != "" {
			return environment[name]
		}
		return fallback
	}
	runtime := installation.RuntimeRoot
	if slices.Contains(owners, "power") {
		helper := value("SUBYARD_POWER_RECONCILER_PATH", filepath.Join(value("SUBYARD_POWER_LIBEXEC_DIR", "/usr/local/libexec/subyard"), "yard-boot-reconcile"))
		unit := value("SUBYARD_POWER_UNIT_PATH", "/etc/systemd/system/subyard-power-reconcile.service")
		if lexists(helper) || lexists(unit) {
			candidates := []string{environment["SUBYARD_DISPATCHER_PATH"], environment["SUBYARD_POWER_ENGINE_SOURCE"]}
			releases, _ := filepath.Glob(runtime + "/releases/*/bin/yard-engine")
			for _, release := range releases {
				if !strings.HasPrefix(filepath.Base(filepath.Dir(filepath.Dir(release))), ".") {
					candidates = append(candidates, release)
				}
			}
			owned, originalHelper, candidateHelper := false, false, false
			powerTemplate, err := template("subyard-power-reconcile.service.in")
			if err != nil {
				return nil, err
			}
			if regularFile(helper) {
				helperDigest, err := fileDigest(helper)
				if err != nil {
					return nil, err
				}
				originalHelper = hex.EncodeToString(helperDigest[:]) == original.EngineSHA256
				candidateHelper = hex.EncodeToString(helperDigest[:]) == original.ExecutingEngineSHA256
				for _, candidate := range candidates {
					if candidate != "" && regularFile(candidate) {
						candidateDigest, err := fileDigest(candidate)
						if err != nil {
							return nil, err
						}
						owned = owned || candidateDigest == helperDigest
					}
				}
			}
			content := []byte{}
			if info, err := os.Stat(unit); err == nil && info.Mode().IsRegular() {
				if content, err = readBounded(unit, 1<<20); err != nil {
					return nil, err
				}
			}
			// Preserve the existing current-template proof first. A historical
			// fallback also covers interrupted replacement of unit and helper,
			// but only the sealed original or executing candidate helper.
			render := func(template []byte) []byte {
				return bytes.ReplaceAll(template, []byte("@SUBYARD_POWER_RECONCILER@"), []byte(helper))
			}
			if !producerTemplateMatches(content, render(powerTemplate), true) && (originalHelper || candidateHelper) && len(original.Templates) > 0 {
				historical, ok := original.Templates["subyard-power-reconcile.service.in"]
				if !ok {
					return nil, errors.New("historical power template is missing")
				}
				powerTemplate = []byte(historical)
			}
			if err := add("system", []string{filepath.Base(unit)}, []producerFile{{path: unit, expected: render(powerTemplate)}, {path: helper, helper: true, owned: owned}}, producerSystemUID); err != nil {
				return nil, err
			}
		}
	}
	if slices.Contains(owners, "keys") {
		directory := environment["SUBYARD_KEYS_SYSTEMD_DIR"]
		if directory == "" {
			home, ok := environment["SUBYARD_OPERATOR_HOME"]
			if !ok {
				return nil, errors.New("operator home is required for key producers")
			}
			directory = filepath.Join(home, ".config/systemd/user")
		}
		unit, timer := filepath.Join(directory, "subyard-keys-sync.service"), filepath.Join(directory, "subyard-keys-sync.timer")
		if lexists(unit) || lexists(timer) {
			content := []byte{}
			if info, err := os.Stat(unit); err == nil && info.Mode().IsRegular() {
				if content, err = os.ReadFile(unit); err != nil {
					return nil, err
				}
			}
			serviceTemplate, ok := original.Templates["subyard-keys-sync.service.in"]
			service := []byte(serviceTemplate)
			if !ok {
				var err error
				if service, err = template("subyard-keys-sync.service.in"); err != nil {
					return nil, err
				}
			}
			timerTemplate, ok := original.Templates["subyard-keys-sync.timer.in"]
			timerBody := []byte(timerTemplate)
			if !ok {
				var err error
				if timerBody, err = template("subyard-keys-sync.timer.in"); err != nil {
					return nil, err
				}
			}
			var expected []byte
			for _, path := range []string{filepath.Join(root, ".build/yard"), filepath.Join(root, "bin/yard"), filepath.Join(runtime, "current/bin/yard")} {
				expected = bytes.ReplaceAll(service, []byte("@YARD_BIN@"), []byte(path))
				if bytes.Equal(content, expected) {
					break
				}
			}
			if err := add("user", []string{"subyard-keys-sync.timer", "subyard-keys-sync.service"}, []producerFile{{path: unit, expected: expected}, {path: timer, expected: timerBody}}, installation.OperatorUID); err != nil {
				return nil, err
			}
		}
	}
	if slices.Contains(owners, "relay") {
		if err := relayProducers(installation, yards, port, add); err != nil {
			return nil, err
		}
	}
	return result, nil
}

var (
	relayName   = regexp.MustCompile(`^subyard-ssh-relay-([1-9][0-9]{0,4})$`)
	relayOwner  = regexp.MustCompile(`(?m)^Environment="SUBYARD_RELAY_OWNER=([^"\r\n]+)"$`)
	relayUID    = regexp.MustCompile(`(?m)^Environment="SUBYARD_RELAY_UID=([0-9]+)"$`)
	relayTarget = regexp.MustCompile(`(?m)^ExecStart=(/usr/lib/systemd/systemd-socket-proxyd|/lib/systemd/systemd-socket-proxyd) ([0-9.]+):22$`)
)

func relayProducers(installation Installation, yards []RecoveryYard, selected int, add func(string, []string, []producerFile, uint32) error) error {
	services, _ := filepath.Glob(relayUnitDirectory + "/subyard-ssh-relay-*.service")
	sockets, _ := filepath.Glob(relayUnitDirectory + "/subyard-ssh-relay-*.socket")
	names := []string{}
	for _, path := range append(services, sockets...) {
		base := filepath.Base(path)
		if name := base[:strings.LastIndex(base, ".")]; !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		match := relayName.FindStringSubmatch(name)
		port := 0
		if match != nil {
			port, _ = strconv.Atoi(match[1])
		}
		if match == nil || port > 65535 {
			return guardRefusal("unrecognized relay artifact")
		}
		if selected != 0 && port != selected {
			continue
		}
		unit := filepath.Join(relayUnitDirectory, name+".service")
		body, err := os.ReadFile(unit)
		if err != nil {
			return err
		}
		if !utf8.Valid(body) {
			return guardRefusal("relay unit is not valid UTF-8")
		}
		content := string(body)
		owner, uid := relayOwner.FindStringSubmatch(content), relayUID.FindStringSubmatch(content)
		marked := owner != nil && uid != nil
		if marked && (owner[1] != installation.DataHome || uid[1] != strconv.FormatUint(uint64(installation.OperatorUID), 10)) {
			continue
		}
		target := relayTarget.FindStringSubmatch(content)
		if target == nil {
			return guardRefusal("unknown relay executable")
		}
		parsed, err := netip.ParseAddr(target[2])
		if err != nil || !parsed.Is4() {
			return guardRefusal("invalid relay address")
		}
		address := parsed.String()
		if !marked {
			proven := false
			for _, yard := range yards {
				if yard.Context.YardKind != domain.YardVM || yard.Context.SSHPort != port {
					continue
				}
				if yard.Snapshot == nil {
					return guardRefusal("legacy relay yard has no snapshot")
				}
				if yard.Snapshot.State.Instance.Devices["eth0"]["ipv4.address"] == address {
					proven = true
					break
				}
			}
			if !proven {
				return guardRefusal("legacy relay has no exact yard/IP ownership")
			}
		}
		socket := fmt.Sprintf("[Unit]\nDescription=Subyard VM SSH loopback relay on port %d\n\n[Socket]\nListenStream=127.0.0.1:%d\nAccept=no\n\n[Install]\nWantedBy=sockets.target\n", port, port)
		expected := fmt.Sprintf("[Unit]\nDescription=Subyard VM SSH relay to %s:22\nAfter=network-online.target\n\n[Service]\n", address)
		if marked {
			expected += fmt.Sprintf("Environment=\"SUBYARD_RELAY_OWNER=%s\"\nEnvironment=\"SUBYARD_RELAY_UID=%s\"\n", owner[1], uid[1])
		}
		expected += fmt.Sprintf("ExecStart=%s %s:22\nNoNewPrivileges=yes\nPrivateTmp=yes\nProtectHome=yes\nProtectSystem=strict\nPrivateDevices=yes\nRestrictAddressFamilies=AF_INET AF_INET6\n", target[1], address)
		files := []producerFile{{path: unit, expected: []byte(expected)}, {path: filepath.Join(relayUnitDirectory, name+".socket"), expected: []byte(socket)}}
		if err := add("system", []string{name + ".socket", name + ".service"}, files, producerSystemUID); err != nil {
			return err
		}
	}
	return nil
}
