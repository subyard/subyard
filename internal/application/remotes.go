package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type RemoteService struct {
	Control ports.RemoteControl
}

func (service RemoteService) Prepare(ctx context.Context, arguments []string) (domain.RemotePrepared, error) {
	if service.Control == nil {
		return domain.RemotePrepared{}, errors.New("remote control port is required")
	}
	action, spec, err := parseRemoteArguments(arguments)
	if err != nil {
		return domain.RemotePrepared{}, err
	}
	prepared := domain.RemotePrepared{Action: action, Spec: spec}
	if action == domain.RemoteList {
		prepared.Records, err = service.Control.List(ctx)
		return prepared, err
	}
	record, exists, err := service.Control.Lookup(ctx, spec.LegacyAlias)
	if err != nil {
		return prepared, err
	}
	if action == domain.RemoteAdd {
		if exists {
			if !record.Remote {
				return prepared, fmt.Errorf("%q is a LOCAL yard — remote add cannot replace it", spec.LegacyAlias)
			}
			if record.Spec.OwnerEndpoint != spec.OwnerEndpoint || record.Spec.OwnerYardName != spec.OwnerYardName {
				return prepared, fmt.Errorf("remote yard %q is already mapped to %s — remove it before rebinding it", spec.LegacyAlias, record.Spec.OwnerEndpoint)
			}
			prepared.Existing = &record
		}
		return service.prepareOwner(ctx, prepared, false)
	}
	if !exists {
		return prepared, fmt.Errorf("no such yard %q", spec.LegacyAlias)
	}
	if !record.Remote {
		return prepared, fmt.Errorf("%q is a LOCAL yard, not a remote one", spec.LegacyAlias)
	}
	prepared.Existing = &record
	prepared.Spec = record.Spec
	if action == domain.RemoteRemove {
		return prepared, nil
	}
	return service.prepareOwner(ctx, prepared, true)
}

func (service RemoteService) prepareOwner(ctx context.Context, prepared domain.RemotePrepared, repair bool) (domain.RemotePrepared, error) {
	owner, err := service.Control.ProbeOwner(ctx, prepared.Spec)
	if err != nil {
		return prepared, fmt.Errorf("probe trusted owner host: %w", err)
	}
	if owner.SSHPort < 1 || owner.SSHPort > 65535 {
		return prepared, errors.New("owner inventory reported an invalid sshPort")
	}
	if owner.DevUser == "" {
		owner.DevUser = "dev"
	}
	if !safeRemoteUser(owner.DevUser) {
		return prepared, errors.New("owner inventory reported an invalid devUser")
	}
	if !strings.EqualFold(owner.State, "RUNNING") {
		return prepared, fmt.Errorf("remote yard state is %s — start it on the owner host first", valueOr(owner.State, "UNKNOWN"))
	}
	if repair && prepared.Existing.SSHPort != owner.SSHPort {
		return prepared, fmt.Errorf("remote ssh port changed from %d to %d; remove and re-add the context", prepared.Existing.SSHPort, owner.SSHPort)
	}
	prepared.Owner = owner
	prepared.Scanned, err = service.Control.ScanYardKeys(ctx, prepared.Spec, owner.SSHPort)
	if err != nil || len(prepared.Scanned) == 0 {
		return prepared, errors.New("the owner host could not scan the yard sshd")
	}
	if prepared.Existing != nil {
		prepared.Recorded, err = service.Control.RecordedYardKeys(ctx, prepared.Spec.LegacyAlias)
		if err != nil {
			return prepared, err
		}
	}
	matched := domain.RemoteKeysOverlap(prepared.Recorded, prepared.Scanned)
	if repair {
		if len(prepared.Recorded) == 0 {
			return prepared, errors.New("there is no recorded yard key; run remote add instead")
		}
		if matched {
			return prepared, errors.New("the recorded yard key already matches the owner-host scan; no rotation is needed")
		}
	} else if len(prepared.Recorded) != 0 && !matched {
		return prepared, fmt.Errorf("yard ssh host key changed; refusing automatic replacement — run: yard remote repair-key %s", prepared.Spec.LegacyAlias)
	}
	return prepared, nil
}

func RemotePolicy(prepared domain.RemotePrepared) domain.CommandPolicy {
	effect := domain.CommandMutate
	if prepared.Action == domain.RemoteList {
		effect = domain.CommandRead
	}
	return domain.CommandPolicy{
		Name: "remote", Effect: effect, RemotePolicy: domain.RemoteOnController,
		Consequences: remoteConsequences(prepared),
	}
}

func RemoteActionPlan(prepared domain.RemotePrepared) (domain.ActionID, domain.ActionDelta, error) {
	switch prepared.Action {
	case domain.RemoteList:
		return "remote.list", domain.ActionDelta{}, nil
	case domain.RemoteAdd:
		return "remote.add", domain.ActionDelta{Changed: true, Consequences: remoteConsequences(prepared)}, nil
	case domain.RemoteRepairKey:
		return "remote.repair-key", domain.ActionDelta{Changed: true, Consequences: remoteConsequences(prepared)}, nil
	case domain.RemoteRemove:
		return "remote.remove", domain.ActionDelta{Changed: true, Consequences: remoteConsequences(prepared)}, nil
	default:
		return "", domain.ActionDelta{}, fmt.Errorf("%w: unknown remote action %q", domain.ErrActionPolicyInvalid, prepared.Action)
	}
}

func remoteConsequences(prepared domain.RemotePrepared) []string {
	switch prepared.Action {
	case domain.RemoteAdd:
		verb := "register"
		if prepared.Existing != nil {
			verb = "refresh"
		}
		lines := []string{
			fmt.Sprintf("%s remote yard %s on %s", verb, prepared.Spec.LegacyAlias, prepared.Spec.OwnerEndpoint),
			"authorize this controller key and atomically install the context and SSH alias",
			"verify the data plane and roll back local files on failure",
		}
		return append(lines, fingerprintLines("yard ssh key", prepared.Scanned)...)
	case domain.RemoteRepairKey:
		lines := fingerprintLines("recorded yard ssh key", prepared.Recorded)
		lines = append(lines, fingerprintLines("new yard ssh key", prepared.Scanned)...)
		return append(lines, "replace only this context's trust pin and restore it if verification fails")
	case domain.RemoteRemove:
		return []string{
			fmt.Sprintf("remove local context and SSH alias for %s", prepared.Spec.LegacyAlias),
			"remove only this yard SSH trust pin; leave the remote host and local project records untouched",
		}
	default:
		return nil
	}
}

type RemoteRunner struct {
	Control  ports.RemoteControl
	Prepared domain.RemotePrepared
}

func (runner RemoteRunner) Run(ctx context.Context, request domain.AdapterRequest, _ io.Reader) (domain.AdapterResult, string, error) {
	if request.Adapter != "remote" || request.Action != string(runner.Prepared.Action) {
		return domain.AdapterResult{}, "", errors.New("remote adapter request does not match its prepared plan")
	}
	result, err := runner.Control.Apply(ctx, runner.Prepared)
	if err != nil {
		return domain.AdapterResult{}, "", err
	}
	return domain.AdapterResult{
		Schema: 1, OperationID: request.OperationID, Status: "ok",
		Output: map[string]any{"message": result.Message, "records": result.Records},
	}, "", nil
}

func parseRemoteArguments(arguments []string) (domain.RemoteAction, domain.RemoteSpec, error) {
	filtered := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		if argument != "-y" && argument != "--yes" {
			filtered = append(filtered, argument)
		}
	}
	if len(filtered) == 0 || filtered[0] == "list" || filtered[0] == "ls" {
		if len(filtered) > 1 {
			return "", domain.RemoteSpec{}, errors.New("remote list takes no arguments")
		}
		return domain.RemoteList, domain.RemoteSpec{}, nil
	}
	action := domain.RemoteAction(filtered[0])
	if action == "rm" {
		action = domain.RemoteRemove
	}
	if action != domain.RemoteAdd && action != domain.RemoteRepairKey && action != domain.RemoteRemove {
		return "", domain.RemoteSpec{}, fmt.Errorf("unknown remote subcommand %q", filtered[0])
	}
	spec := domain.RemoteSpec{}
	for index := 1; index < len(filtered); index++ {
		argument := filtered[index]
		switch {
		case argument == "--yard":
			index++
			if index >= len(filtered) {
				return "", spec, errors.New("--yard needs a name")
			}
			spec.OwnerYardName = filtered[index]
		case strings.HasPrefix(argument, "--yard="):
			spec.OwnerYardName = strings.TrimPrefix(argument, "--yard=")
		case strings.HasPrefix(argument, "-"):
			return "", spec, fmt.Errorf("unknown option %q", argument)
		case spec.LegacyAlias == "":
			spec.LegacyAlias = argument
		case spec.OwnerEndpoint == "" && action == domain.RemoteAdd:
			spec.OwnerEndpoint = argument
		default:
			return "", spec, fmt.Errorf("unexpected argument %q", argument)
		}
	}
	if !domain.SafeName(spec.LegacyAlias) {
		return "", spec, fmt.Errorf("invalid remote context name %q", spec.LegacyAlias)
	}
	if spec.OwnerYardName != "" && !domain.SafeName(spec.OwnerYardName) {
		return "", spec, fmt.Errorf("invalid owner yard name %q", spec.OwnerYardName)
	}
	if action != domain.RemoteAdd && spec.OwnerYardName != "" {
		return "", spec, errors.New("--yard is valid only with remote add")
	}
	if action == domain.RemoteAdd && !domain.SafeSSHTarget(spec.OwnerEndpoint) {
		return "", spec, fmt.Errorf("invalid ssh destination %q", spec.OwnerEndpoint)
	}
	return action, spec, nil
}

func safeRemoteUser(value string) bool {
	return value != "" && value[0] != '-' && strings.IndexFunc(value, func(char rune) bool {
		return !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') &&
			!(char >= '0' && char <= '9') && !strings.ContainsRune("_.-", char)
	}) < 0
}

func fingerprintLines(label string, keys []domain.RemoteKey) []string {
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, label+": "+key.Fingerprint)
	}
	return lines
}
