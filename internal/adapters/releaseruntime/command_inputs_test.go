package releaseruntime

import (
	"maps"
	"path/filepath"
	"reflect"
	"testing"
)

// Adapter fixtures describe original process inputs, never loaded yard settings.
func fixtureCommandInputs(values map[string]string) CommandInputs {
	values = maps.Clone(values)
	for key, value := range map[string]string{
		"DEV_UID": "1000", "SSH_PORT": "2222", "SHIFT_MODE": "shift",
		"DEV_SUDO": "0", "FORWARD_SSH_AGENT": "0",
		"SUBYARD_HOME":          filepath.Join(values["HOME"], "data"),
		"STORAGE_PATH":          filepath.Join(values["HOME"], "storage"),
		"HOST_BASE":             filepath.Join(values["HOME"], "host"),
		"RESTRICTED_DISK_PATHS": filepath.Join(values["HOME"], "host"),
	} {
		if _, present := values[key]; !present {
			values[key] = value
		}
	}
	return CaptureCommandInputs(commandEnvironment(values), nil)
}

func TestCommandInputsKeepOriginalOverridesAndInheritedIDs(t *testing.T) {
	original := []string{"HOST_MOUNTS=first", "HOST_MOUNTS=", "AGENTS=codex", "PATH=/fixture/bin", "SUBYARD_SUDO_PREAUTHORIZED=1", "SUBYARD_PRESERVE_VM_REPORTING=1"}
	inputs := CaptureCommandInputs(original, func(name string) bool { return name == "HOST_MOUNTS" })
	original[1] = "HOST_MOUNTS=changed"
	ids := inputs.InheritedSettingIDs()
	ids[0] = "changed"
	values := inputs.environment(RuntimeContext{ConfigHome: "/fixture/config", OperationID: "operation"})
	if values["HOST_MOUNTS"] != "" || values["AGENTS"] != "codex" || values["PATH"] != "/fixture/bin" || values["SUBYARD_CONFIG_HOME"] != "/fixture/config" || values["SUBYARD_OPERATION_ID"] != "operation" {
		t.Fatal("original overrides or curated context changed")
	}
	if _, ok := values["HOST_MOUNTS"]; !ok {
		t.Fatal("empty command override disappeared")
	}
	if _, ok := values["SUBYARD_SUDO_PREAUTHORIZED"]; ok {
		t.Fatal("sudo preauthorization forwarded")
	}
	if _, ok := values["SUBYARD_PRESERVE_VM_REPORTING"]; ok {
		t.Fatal("internal VM preservation signal forwarded")
	}
	values["HOST_MOUNTS"] = "changed"
	if !reflect.DeepEqual(inputs.InheritedSettingIDs(), []string{"HOST_MOUNTS"}) || inputs.environment(RuntimeContext{})["HOST_MOUNTS"] != "" {
		t.Fatal("captured command inputs were mutable")
	}
}
