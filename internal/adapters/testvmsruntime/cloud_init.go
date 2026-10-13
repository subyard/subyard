package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
)

const cloudInitDevice = "subyard-cloud-init"

// Incus generates a NoCloud cidata disk from the instance's existing cloud-init
// config. Attach it only to a stopped owned VM; never edit shared profiles or
// replace a colliding device. startVM uses this for both builders and leases.
func (runtime *Runtime) ensureCloudInitDisk(ctx context.Context, vm string) error {
	const source = "cloud-init:config"
	want := map[string]string{"type": "disk", "source": source}
	commandError := func(err error, diagnostic string) error {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		message := fmt.Sprintf("%s cloud-init device %s", vm, diagnostic)
		var native *CommandError
		if errors.As(err, &native) {
			return &CommandError{Name: "incus", ExitCode: native.ExitCode,
				Duration: native.Duration, Message: message}
		}
		return errors.New(message)
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
