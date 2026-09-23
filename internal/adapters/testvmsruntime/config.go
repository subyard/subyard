package testvmsruntime

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"golang.org/x/crypto/ssh"
)

const (
	DefaultConfigPath    = "/etc/subyard/test-vms.env"
	DefaultInstalledPath = "/usr/local/libexec/subyard/test-vms-inner"
	managedMarker        = "test-vms-v1"
	agentKeyMarker       = "subyard-managed-e2e-agent"
)

var (
	safeName  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	safeUser  = regexp.MustCompile(`^[a-z0-9_][a-z0-9_-]*$`)
	safeImage = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]+$`)
	sizeValue = regexp.MustCompile(`^([1-9][0-9]*)(MiB|GiB)$`)
)

type Config struct {
	Environment         *EnvironmentSpec
	DiskBudget          string
	CacheBudget         string
	DiskReserve         string
	MemoryReserve       string
	VMOverhead          string
	RecipeRoot          string
	Enabled             bool
	Project             string
	Network             string
	Prefix              string
	Image               string
	CPU                 int
	Memory              string
	Disk                string
	SlotCount           int
	BootTimeout         time.Duration
	DevUser             string
	StateDir            string
	BrokerSource        string
	AgentUser           string
	AgentPublicKey      string
	AgentHome           string
	AgentAuthorizedKeys string
	StatusCommand       string
	Incus               string
}

func LoadConfig(path string) (Config, error) {
	if path == "" {
		path = DefaultConfigPath
	}
	values, err := config.ReadAssignments(path)
	if err != nil {
		return Config{}, fmt.Errorf("read test-vms config: %w", err)
	}
	return ConfigFromValues(values)
}

func ConfigFromValues(values map[string]string) (Config, error) {
	value := func(name, fallback string) string {
		if values[name] != "" {
			return values[name]
		}
		return fallback
	}
	enabled := value("NESTED_E2E_VMS", "0")
	if enabled != "0" && enabled != "1" {
		return Config{}, errors.New("invalid NESTED_E2E_VMS")
	}
	cpu, err := positiveInt(value("E2E_VM_CPU", "4"), "E2E_VM_CPU")
	if err != nil {
		return Config{}, err
	}
	slots, err := positiveInt(value("E2E_VM_SLOT_COUNT", "2"), "E2E_VM_SLOT_COUNT")
	if err != nil {
		return Config{}, err
	}
	boot, err := boundedSeconds(value("E2E_VM_BOOT_TIMEOUT", "300"), 30, 1800,
		"E2E_VM_BOOT_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	result := Config{
		DiskBudget:    value("E2E_DISK_BUDGET", "0GiB"),
		CacheBudget:   value("E2E_CACHE_BUDGET", "24GiB"),
		DiskReserve:   value("E2E_DISK_RESERVE", "5GiB"),
		MemoryReserve: value("E2E_MEMORY_RESERVE", "2GiB"),
		VMOverhead:    value("E2E_VM_OVERHEAD", "512MiB"),
		RecipeRoot:    value("E2E_RECIPE_ROOT", "/usr/local/libexec/subyard/e2e-recipes"),
		Enabled:       enabled == "1", Project: value("E2E_VM_PROJECT", "subyard-e2e-vms"),
		Network: value("E2E_VM_NETWORK", "incusbr0"),
		Prefix:  value("E2E_VM_PREFIX", "e2e-vm"), Image: value("E2E_VM_IMAGE", "images:debian/13/cloud"),
		CPU: cpu, Memory: value("E2E_VM_MEMORY", "4GiB"), Disk: value("E2E_VM_DISK", "20GiB"),
		SlotCount: slots, BootTimeout: boot, DevUser: value("DEV_USER", "dev"),
		StateDir:       value("E2E_VM_STATE_DIR", "/var/lib/subyard/test-vms"),
		BrokerSource:   value("E2E_BROKER_SOURCE", "test-yard"),
		AgentUser:      value("E2E_AGENT_USER", "subyard-e2e-agent"),
		AgentPublicKey: values["E2E_AGENT_PUBLIC_KEY"],
		AgentHome:      value("E2E_AGENT_HOME", "/var/lib/subyard/e2e-agent"),
		StatusCommand: value("E2E_AGENT_STATUS_COMMAND",
			"sudo -n "+DefaultInstalledPath+" _test-vms-facade"),
		Incus: value("SUBYARD_INNER_INCUS", "incus"),
	}
	result.AgentAuthorizedKeys = value("E2E_AGENT_AUTHORIZED_KEYS",
		filepath.Join(result.AgentHome, ".ssh", "authorized_keys"))
	if err := result.Validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

func (cfg Config) Validate() error {
	for index, value := range []string{cfg.DiskBudget, cfg.CacheBudget, cfg.DiskReserve, cfg.MemoryReserve, cfg.VMOverhead} {
		if index == 0 && value == "0GiB" {
			continue
		}
		if value != "" {
			if _, err := sizeMiB(value); err != nil {
				return errors.New("invalid test environment budget")
			}
		}
	}
	if !safeName.MatchString(cfg.Project) {
		return fmt.Errorf("unsafe E2E_VM_PROJECT %q", cfg.Project)
	}
	if !safeName.MatchString(cfg.Network) {
		return fmt.Errorf("unsafe E2E_VM_NETWORK %q", cfg.Network)
	}
	if !safeName.MatchString(cfg.Prefix) {
		return fmt.Errorf("unsafe E2E_VM_PREFIX %q", cfg.Prefix)
	}
	if !safeImage.MatchString(cfg.Image) {
		return fmt.Errorf("unsafe E2E_VM_IMAGE %q", cfg.Image)
	}
	if _, err := sizeMiB(cfg.Memory); err != nil {
		return fmt.Errorf("E2E_VM_MEMORY must use MiB or GiB")
	}
	disk, err := sizeMiB(cfg.Disk)
	if err != nil {
		return fmt.Errorf("E2E_VM_DISK must use MiB or GiB")
	}
	if disk < 10*1024 {
		return errors.New("E2E_VM_DISK must be at least 10GiB")
	}
	if !safeUser.MatchString(cfg.DevUser) {
		return fmt.Errorf("unsafe DEV_USER %q", cfg.DevUser)
	}
	if !safeUser.MatchString(cfg.AgentUser) {
		return fmt.Errorf("unsafe E2E_AGENT_USER %q", cfg.AgentUser)
	}
	if !safeLeaseText(cfg.BrokerSource, 96) || cfg.BrokerSource == "" {
		return fmt.Errorf("unsafe E2E_BROKER_SOURCE %q", cfg.BrokerSource)
	}
	for name, path := range map[string]string{
		"E2E_VM_STATE_DIR": cfg.StateDir, "E2E_AGENT_HOME": cfg.AgentHome,
	} {
		if !within(path, "/var/lib/subyard") {
			return fmt.Errorf("unsafe %s %q", name, path)
		}
	}
	if cfg.StatusCommand != "sudo -n "+DefaultInstalledPath+" _test-vms-facade" {
		return fmt.Errorf("unsafe E2E_AGENT_STATUS_COMMAND %q", cfg.StatusCommand)
	}
	if cfg.AgentPublicKey != "" {
		if strings.ContainsAny(cfg.AgentPublicKey, "\r\n") {
			return errors.New("E2E_AGENT_PUBLIC_KEY must be one line")
		}
		key, err := parsePublicKey(cfg.AgentPublicKey)
		if err != nil || key.Type() != ssh.KeyAlgoED25519 {
			return errors.New("E2E_AGENT_PUBLIC_KEY must be an Ed25519 public key")
		}
	}
	return nil
}

func (cfg Config) vm(index int) string    { return cfg.Prefix + "-" + strconv.Itoa(index) }
func (cfg Config) keyPath() string        { return filepath.Join(cfg.StateDir, "id_ed25519") }
func (cfg Config) knownHosts() string     { return filepath.Join(cfg.StateDir, "known_hosts") }
func (cfg Config) failureLog() string     { return filepath.Join(cfg.StateDir, "last-failure.log") }
func (cfg Config) stateMarker() string    { return filepath.Join(cfg.StateDir, ".subyard-managed") }
func (cfg Config) keyRevision() string    { return filepath.Join(cfg.StateDir, "worker-key-v2") }
func (cfg Config) revokedKey() string     { return filepath.Join(cfg.StateDir, "revoked-worker.pub") }
func (cfg Config) leaseState() string     { return filepath.Join(cfg.StateDir, "leases.json") }
func (cfg Config) LeaseStatePath() string { return cfg.leaseState() }

func positiveInt(value, name string) (int, error) {
	number, err := strconv.Atoi(value)
	if err != nil || number < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return number, nil
}

func boundedSeconds(value string, minimum, maximum int, name string) (time.Duration, error) {
	number, err := strconv.Atoi(value)
	if err != nil || number < minimum || number > maximum {
		return 0, fmt.Errorf("%s must be from %d to %d", name, minimum, maximum)
	}
	return time.Duration(number) * time.Second, nil
}

func sizeMiB(value string) (int, error) {
	match := sizeValue.FindStringSubmatch(value)
	if match == nil {
		return 0, errors.New("invalid size")
	}
	number, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil || number == 0 || number > 1<<30 {
		return 0, errors.New("size outside supported range")
	}
	if match[2] == "GiB" {
		number *= 1024
	}
	if number > 1<<30 {
		return 0, errors.New("size outside supported range")
	}
	return int(number), nil
}

func doubleSize(value string) string {
	match := sizeValue.FindStringSubmatch(value)
	number, _ := strconv.Atoi(match[1])
	return strconv.Itoa(number*2) + match[2]
}

func within(path, root string) bool {
	clean := filepath.Clean(path)
	root = filepath.Clean(root)
	return clean != root && strings.HasPrefix(clean, root+string(filepath.Separator))
}

func parsePublicKey(value string) (ssh.PublicKey, error) {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(value)))
	return key, err
}

func normalizedPublicKey(value string) (string, error) {
	key, err := parsePublicKey(value)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), nil
}
