package incusclient

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/lxc/incus/v6/shared/api"
)

const ownerCreateTokenKey = "user.subyard.owner_create"

func ownerImageProjectRequest(p ports.PreparedImageProject) (api.ProjectsPost, error) {
	request := api.ProjectsPost{Name: p.Project, ProjectPut: api.ProjectPut{Config: map[string]string{}}}
	if p.SharedImages {
		request.Config["features.images"] = "false"
	}
	if p.InitialConfig == nil {
		return request, nil
	}
	if len(p.InitialConfig) == 0 || len(p.InitialConfig) > 32<<20 || p.InitialConfig[len(p.InitialConfig)-1] != 0 {
		return request, domain.ErrPlanStale
	}
	// This is the existing 02 renderer's closed output grammar, not another
	// policy renderer. Values remain exactly the original captured bytes.
	keys := strings.Fields("user.subyard.installation user.subyard.operator_uid user.subyard.context user.subyard.data_home user.subyard.config_home user.subyard.yard_name user.subyard.instance_name user.subyard.host_base user.subyard.bridge user.subyard.srv_pool user.subyard.srv_volume user.subyard.storage_pool user.subyard.storage_path restricted restricted.containers.nesting restricted.containers.privilege restricted.containers.interception restricted.devices.disk restricted.devices.unix-char restricted.devices.proxy")
	seen := map[string]bool{}
	for _, row := range bytes.Split(p.InitialConfig[:len(p.InitialConfig)-1], []byte{0}) {
		key, value, ok := strings.Cut(string(row), "=")
		if !ok || seen[key] || key != "restricted.virtual-machines.lowlevel" && !slices.Contains(keys, key) {
			return request, domain.ErrPlanStale
		}
		seen[key], request.Config[key] = true, value
	}
	for _, key := range keys {
		if !seen[key] {
			return request, domain.ErrPlanStale
		}
	}
	var context domain.Context
	if request.Config["user.subyard.installation"] != "1" || request.Config["user.subyard.operator_uid"] != strconv.Itoa(os.Getuid()) || json.Unmarshal([]byte(request.Config["user.subyard.context"]), &context) != nil || context.IncusProject != p.Project || context.YardName != request.Config["user.subyard.yard_name"] || context.YardInstanceName != request.Config["user.subyard.instance_name"] {
		return request, domain.ErrPlanStale
	}
	return request, nil
}
