package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

const cloudInitDevice = "subyard-cloud-init"

// Incus generates a NoCloud cidata disk from the instance's existing cloud-init
// config. Attach it only to a stopped owned VM; never edit shared profiles or
// replace a colliding device. startVM uses this for both builders and leases.
func (runtime *Runtime) ensureCloudInitDisk(ctx context.Context, vm string) error {
	const source = "cloud-init:config"
	want := map[string]string{"type": "disk", "source": source}
	commandError := func(err error, diagnostic string) error {
		return cloudInitCommandError(ctx, err, vm+" cloud-init device "+diagnostic)
	}
	read := func() (bool, error) {
		body, err := runtime.incus(ctx, "query", "/1.0/instances/"+url.PathEscape(vm)+"?project="+url.QueryEscape(runtime.Config.Project))
		if err != nil {
			return false, commandError(err, "inventory unavailable")
		}
		var instance struct {
			Type            string                       `json:"type"`
			Config          map[string]string            `json:"config"`
			Devices         map[string]map[string]string `json:"devices"`
			ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
		}
		if json.Unmarshal([]byte(body), &instance) != nil || instance.Type != "virtual-machine" ||
			instance.Config["user.subyard.managed"] != managedMarker || instance.Devices == nil || instance.ExpandedDevices == nil {
			return false, fmt.Errorf("%s cloud-init device inventory invalid or unowned", vm)
		}
		if runtime.allocation != nil && (instance.Config["user.subyard.generation"] != strconv.FormatUint(runtime.allocation.ResourceGeneration, 10) ||
			instance.Config["user.subyard.lease-epoch"] != strconv.FormatUint(runtime.allocation.LeaseEpoch, 10)) {
			return false, fmt.Errorf("%s cloud-init device allocation mismatch", vm)
		}
		local, expanded := instance.Devices[cloudInitDevice], instance.ExpandedDevices[cloudInitDevice]
		if (local != nil && !reflect.DeepEqual(local, want)) || !reflect.DeepEqual(local, expanded) {
			return false, fmt.Errorf("%s cloud-init device is inherited or divergent", vm)
		}
		for _, devices := range []map[string]map[string]string{instance.Devices, instance.ExpandedDevices} {
			for name, device := range devices {
				if name != cloudInitDevice && device["source"] == source {
					return false, fmt.Errorf("%s cloud-init source is occupied", vm)
				}
			}
		}
		return local != nil, nil
	}
	present, err := read()
	if err != nil || present {
		return err
	}
	if _, err := runtime.incus(ctx, "config", "device", "add", vm, cloudInitDevice, "disk", "source="+source, "--project", runtime.Config.Project); err != nil {
		return commandError(err, "attachment failed")
	}
	present, err = read()
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%s cloud-init device attachment was not verified", vm)
	}
	return nil
}

// CIDATA already carries the builder's cloud config. Remove only the duplicate
// NoCloud seed templates: Incus' agent otherwise seeds them and reboots before
// serving its API. Instance metadata is also carried into published images.
func (runtime *Runtime) prepareBuilderCloudInit(ctx context.Context, registry *ImageRegistry) error {
	if registry == nil || registry.Build == nil || runtime.Config.Project != registry.Build.Project || runtime.Config.Prefix != "e2e-base" {
		return errors.New("cloud-init preparation requires the private builder")
	}
	vm := runtime.Config.vm(1)
	instancePath := "/1.0/instances/" + url.PathEscape(vm) + "?project=" + url.QueryEscape(runtime.Config.Project)
	metadataPath := "/1.0/instances/" + url.PathEscape(vm) + "/metadata?project=" + url.QueryEscape(runtime.Config.Project)
	var uuid string
	guard := func() error {
		body, err := runtime.incus(ctx, "query", instancePath)
		if err != nil {
			return cloudInitCommandError(ctx, err, "builder cloud-init identity unavailable")
		}
		var instance struct {
			Type, Status string
			Stateful     *bool
			Config       map[string]string
		}
		if json.Unmarshal([]byte(body), &instance) != nil || instance.Type != "virtual-machine" || instance.Status != "Stopped" ||
			instance.Stateful == nil || *instance.Stateful || instance.Config["user.subyard.managed"] != managedMarker ||
			instance.Config["user.subyard.base_owner"] != registry.Owner || instance.Config["user.subyard.base_key"] != registry.Build.Key {
			return errors.New("builder cloud-init identity is invalid, unowned or not stopped")
		}
		got := instance.Config["volatile.uuid"]
		valid, _ := regexp.MatchString(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`, got)
		if !valid || (uuid != "" && uuid != got) {
			return errors.New("builder cloud-init UUID unavailable or changed")
		}
		uuid = got
		return nil
	}
	if err := guard(); err != nil {
		return err
	}
	if err := runtime.ensureCloudInitDisk(ctx, vm); err != nil {
		return err
	}
	read := func() (map[string]json.RawMessage, error) {
		body, err := runtime.incus(ctx, "query", metadataPath)
		if err != nil {
			return nil, cloudInitCommandError(ctx, err, "builder cloud-init metadata unavailable")
		}
		var metadata map[string]json.RawMessage
		if json.Unmarshal([]byte(body), &metadata) != nil || metadata == nil {
			return nil, errors.New("builder cloud-init metadata invalid")
		}
		return metadata, nil
	}
	metadata, err := read()
	if err != nil {
		return err
	}
	var templates map[string]json.RawMessage
	if json.Unmarshal(metadata["templates"], &templates) != nil || templates == nil {
		return errors.New("builder cloud-init template inventory invalid")
	}
	for target := range templates {
		switch target {
		case "/var/lib/cloud/seed/nocloud-net/meta-data", "/var/lib/cloud/seed/nocloud-net/user-data",
			"/var/lib/cloud/seed/nocloud-net/vendor-data", "/var/lib/cloud/seed/nocloud-net/network-config":
			delete(templates, target)
		default:
			if strings.HasPrefix(target, "/var/lib/cloud/seed/") {
				return errors.New("builder cloud-init seed template is unsupported")
			}
		}
	}
	metadata["templates"], _ = json.Marshal(templates)
	body, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	// PUT has no enforced ETag in Incus 6.0.6. The build lock, stopped private
	// instance and identity rechecks bound this update; never use PATCH omission.
	if err := guard(); err != nil {
		return err
	}
	if _, err := runtime.incus(ctx, "query", metadataPath, "-X", "PUT", "-d", string(body)); err != nil {
		return cloudInitCommandError(ctx, err, "builder cloud-init metadata update failed")
	}
	current, err := read()
	if err != nil {
		return err
	}
	gotBody, _ := json.Marshal(current)
	var got, want any
	if json.Unmarshal(gotBody, &got) != nil || json.Unmarshal(body, &want) != nil || !reflect.DeepEqual(got, want) {
		return errors.New("builder cloud-init metadata update was not verified")
	}
	return guard()
}

func cloudInitCommandError(ctx context.Context, err error, message string) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var native *CommandError
	if errors.As(err, &native) {
		return &CommandError{Name: "incus", ExitCode: native.ExitCode, Duration: native.Duration, Message: message}
	}
	return errors.New(message)
}
