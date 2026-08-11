package credentialruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/credential"
	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/crypto/ssh"
)

type Prepared struct {
	Action       domain.ActionID
	Changed      bool
	Consequences []string
	run          func(context.Context) error
}

func (prepared Prepared) Execute(ctx context.Context) error {
	if prepared.run == nil {
		return errors.New("credential operation is not executable")
	}
	return prepared.run(ctx)
}

func (runtime *Runtime) Prepare(ctx context.Context, entry string, arguments []string) (Prepared, error) {
	arguments = append([]string(nil), arguments...)
	switch entry {
	case "":
		return runtime.preparePublic(ctx, arguments)
	case "_exchange":
		return runtime.prepareExchange(arguments)
	case "_auto-worker":
		return runtime.prepareAutoWorker(arguments)
	case "_init-store":
		if len(arguments) != 0 {
			return Prepared{}, errors.New("credential store initializer takes no arguments")
		}
		changed := !runtime.Initialized()
		return runtime.mutation(
			"keys.init-store", changed,
			[]string{"initialize the host-only encrypted credential ledger"},
			func(ctx context.Context) error {
				if !changed {
					return nil
				}
				return runtime.Initialize(ctx)
			},
		), nil
	default:
		return Prepared{}, fmt.Errorf("unknown credential entrypoint %q", entry)
	}
}

func (runtime *Runtime) preparePublic(ctx context.Context, arguments []string) (Prepared, error) {
	if len(arguments) == 0 || arguments[0] == "-h" || arguments[0] == "--help" {
		return runtime.readOperation("keys.help", runtime.help), nil
	}
	subcommand := arguments[0]
	arguments = arguments[1:]
	switch subcommand {
	case "list":
		if err := onlyYes(arguments); err != nil {
			return Prepared{}, fmt.Errorf("keys list: %w", err)
		}
		return runtime.readOperation("keys.list", runtime.list), nil
	case "status":
		if err := onlyYes(arguments); err != nil {
			return Prepared{}, fmt.Errorf("keys status: %w", err)
		}
		return runtime.readOperation("keys.status", runtime.status), nil
	case "history":
		wanted, err := optionalValue(arguments)
		if err != nil {
			return Prepared{}, fmt.Errorf("keys history: %w", err)
		}
		return runtime.readOperation("keys.history", func(ctx context.Context) error { return runtime.history(ctx, wanted) }), nil
	case "check-exclusive":
		zone, err := requiredValue(arguments, "check-exclusive needs a zone")
		if err != nil || !validZone(zone) {
			return Prepared{}, errors.New("check-exclusive needs a valid zone")
		}
		return runtime.readOperation("keys.check-exclusive", func(ctx context.Context) error { return runtime.checkExclusive(ctx, zone) }), nil
	case "add":
		return runtime.prepareAdd(ctx, arguments)
	case "import":
		return runtime.prepareImport(ctx, arguments)
	case "rotate":
		return runtime.prepareRotate(ctx, arguments)
	case "rollback":
		return runtime.prepareRollback(ctx, arguments)
	case "revoke", "delete":
		return runtime.prepareTerminal(ctx, subcommand, arguments)
	case "resolve":
		return runtime.prepareResolve(ctx, arguments)
	case "materialize":
		return runtime.prepareMaterialize(ctx, arguments)
	case "sync":
		return runtime.prepareSync(ctx, arguments)
	case "auto-sync":
		return runtime.prepareAutoSync(arguments)
	case "trust":
		return runtime.prepareTrust(ctx, arguments)
	case "untrust":
		return runtime.prepareUntrust(arguments)
	case "move":
		return runtime.prepareMove(ctx, arguments)
	default:
		return Prepared{}, fmt.Errorf("unknown keys command %q (run: yard keys --help)", subcommand)
	}
}

func (runtime *Runtime) readOperation(action domain.ActionID, run func(context.Context) error) Prepared {
	return Prepared{Action: action, run: run}
}

func (runtime *Runtime) mutation(
	action domain.ActionID,
	changed bool,
	consequences []string,
	run func(context.Context) error,
) Prepared {
	if !changed {
		consequences = nil
	}
	return Prepared{Action: action, Changed: changed, Consequences: consequences, run: run}
}

func (runtime *Runtime) help(context.Context) error {
	fmt.Fprint(runtime.config.Stdout, `Usage: yard keys <command> [args]

Host-side encrypted credential ledger:
  trust @peer [--manual-only]
  untrust @peer
  add <label> [--kind K] [--zone Z] [--consumer C] [--file PATH] [--local-only] [--exclusive]
  import <file> [--label L] [--kind K] [--zone Z] [--consumer C] [--dry-run]
  list | status | history [credential-id]
  sync [@peer|--all] [--now]
  auto-sync status|pause|resume [@peer|--all]
  materialize [zone|--all]
  rotate <credential-id> [--file PATH]
  rollback <credential-id> <revision-id>
  revoke <credential-id> | delete <credential-id>
  resolve <credential-id> --choose <revision>|--rotate [--file PATH]
  move <credential-id> @peer

Secret values are read only after confirmation from a protected file, stdin or a silent TTY.
`)
	return nil
}

type addOptions struct {
	label, kind, zone, consumer, source string
	localOnly, exclusive                bool
}

func parseAdd(arguments []string) (addOptions, error) {
	options := addOptions{kind: "opaque", zone: "global", consumer: "none"}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch argument {
		case "--kind", "--zone", "--consumer", "--file":
			index++
			if index >= len(arguments) {
				return options, fmt.Errorf("%s needs a value", argument)
			}
			switch argument {
			case "--kind":
				options.kind = arguments[index]
			case "--zone":
				options.zone = arguments[index]
			case "--consumer":
				options.consumer = arguments[index]
			case "--file":
				options.source = arguments[index]
			}
		case "--local-only":
			options.localOnly = true
		case "--exclusive":
			options.exclusive = true
		case "-y", "--yes":
		default:
			if strings.HasPrefix(argument, "-") {
				return options, fmt.Errorf("unknown option %q", argument)
			}
			if options.label != "" {
				return options, errors.New("add takes one label")
			}
			options.label = argument
		}
	}
	if options.label == "" {
		return options, errors.New("keys add needs a label")
	}
	if err := validateClassification(options.label, options.kind, options.zone, options.consumer); err != nil {
		return options, err
	}
	return options, nil
}

func (runtime *Runtime) prepareAdd(ctx context.Context, arguments []string) (Prepared, error) {
	options, err := parseAdd(arguments)
	if err != nil {
		return Prepared{}, err
	}
	if err := runtime.requireInitialized(); err != nil {
		return Prepared{}, err
	}
	if options.source != "" {
		options.source, err = runtime.validateImportPath(options.source)
		if err != nil {
			return Prepared{}, err
		}
	}
	if conflict, err := runtime.consumerConflict(ctx, options.consumer, options.zone); err != nil {
		return Prepared{}, err
	} else if conflict != "" {
		return Prepared{}, fmt.Errorf("consumer %s/%s is already owned by %s; rotate that credential instead",
			options.consumer, options.zone, conflict)
	}
	consequences := []string{
		fmt.Sprintf("add encrypted credential %q", options.label),
		fmt.Sprintf("kind=%s zone=%s consumer=%s local-only=%t exclusive=%t",
			options.kind, options.zone, options.consumer, options.localOnly, options.exclusive),
		"read the protected value only after confirmation and publish one signed immutable revision",
	}
	return runtime.mutation("keys.add", true, consequences, func(ctx context.Context) error {
		return runtime.withLock(ctx, func() error {
			payload, err := runtime.capturePayload(options.source)
			if err != nil {
				return err
			}
			defer clear(payload)
			if err := runtime.rejectProductionPayload(payload); err != nil {
				return err
			}
			credentialID, err := runtime.add(ctx, options, payload)
			if err != nil {
				return err
			}
			fmt.Fprintf(runtime.config.Stderr, "  [ ok ] added credential %s (%s)\n", credentialID, options.label)
			return nil
		})
	}), nil
}

func (runtime *Runtime) add(ctx context.Context, options addOptions, payload []byte) (string, error) {
	scope := sharedLedger
	if options.localOnly {
		scope = localLedger
	} else if err := runtime.refreshShared(ctx); err != nil {
		return "", err
	}
	identity, err := runtime.Identity()
	if err != nil {
		return "", err
	}
	recipients := []string{identity.ActorID}
	if scope == sharedLedger {
		recipients, err = runtime.recipientActors()
		if err != nil {
			return "", err
		}
	}
	spec := revisionSpec{
		Label: options.label, Kind: options.kind, Zone: options.zone, Consumer: options.consumer,
		State: "active", RecipientActors: recipients, Exclusive: options.exclusive,
		Syncable: scope == sharedLedger,
	}
	if options.exclusive {
		spec.AuthorityHost = identity.ActorID
		spec.AssignedYard = identity.ActorID + "/" + runtime.config.Context
		spec.AssignmentEpoch = 1
	}
	metadata, err := runtime.publish(ctx, scope, spec, payload)
	return metadata.CredentialID, err
}

type importOptions struct {
	addOptions
	dryRun bool
}

func (runtime *Runtime) prepareImport(ctx context.Context, arguments []string) (Prepared, error) {
	options := importOptions{addOptions: addOptions{kind: "file"}}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch argument {
		case "--label", "--kind", "--zone", "--consumer":
			index++
			if index >= len(arguments) {
				return Prepared{}, fmt.Errorf("%s needs a value", argument)
			}
			switch argument {
			case "--label":
				options.label = arguments[index]
			case "--kind":
				options.kind = arguments[index]
			case "--zone":
				options.zone = arguments[index]
			case "--consumer":
				options.consumer = arguments[index]
			}
		case "--local-only":
			options.localOnly = true
		case "--exclusive":
			options.exclusive = true
		case "--dry-run":
			options.dryRun = true
		case "-y", "--yes":
		default:
			if strings.HasPrefix(argument, "-") {
				return Prepared{}, fmt.Errorf("unknown option %q", argument)
			}
			if options.source != "" {
				return Prepared{}, errors.New("import takes one source file")
			}
			options.source = argument
		}
	}
	if options.source == "" {
		return Prepared{}, errors.New("keys import needs a source file")
	}
	real, err := runtime.validateImportPath(options.source)
	if err != nil {
		return Prepared{}, err
	}
	options.source = real
	if options.label == "" {
		options.label = filepath.Base(real)
	}
	if options.consumer == "" {
		options.consumer = runtime.detectConsumer(real)
	}
	if options.zone == "" {
		options.zone = runtime.detectZone(real)
	}
	if err := validateClassification(options.label, options.kind, options.zone, options.consumer); err != nil {
		return Prepared{}, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return Prepared{}, err
	}
	fmt.Fprintf(runtime.config.Stdout,
		"source: %s\nlabel: %s\nkind: %s\nzone: %s\nconsumer: %s\npolicy: %s\nexclusive: %t\nsize: %d bytes\n",
		real, options.label, options.kind, options.zone, options.consumer,
		map[bool]string{true: "local-only", false: "syncable"}[options.localOnly], options.exclusive, info.Size())
	if options.dryRun {
		return runtime.readOperation("keys.import-dry-run", func(context.Context) error {
			fmt.Fprintln(runtime.config.Stdout, "dry-run only; no value was read and no ledger changed")
			return nil
		}), nil
	}
	if err := runtime.requireInitialized(); err != nil {
		return Prepared{}, err
	}
	if conflict, err := runtime.consumerConflict(ctx, options.consumer, options.zone); err != nil {
		return Prepared{}, err
	} else if conflict != "" {
		return Prepared{}, fmt.Errorf("consumer %s/%s is already owned by %s; rotate it instead",
			options.consumer, options.zone, conflict)
	}
	return runtime.mutation("keys.import", true, []string{
		fmt.Sprintf("import static credential file %q", options.label),
		"read the protected source only after confirmation",
		"encrypt it into the host-only ledger and keep the source file",
	}, func(ctx context.Context) error {
		return runtime.withLock(ctx, func() error {
			payload, err := runtime.capturePayload(options.source)
			if err != nil {
				return err
			}
			defer clear(payload)
			if err := runtime.rejectProductionPayload(payload); err != nil {
				return err
			}
			credentialID, err := runtime.add(ctx, options.addOptions, payload)
			if err != nil {
				return err
			}
			fmt.Fprintf(runtime.config.Stderr, "  [ ok ] imported credential %s; source file was kept\n", credentialID)
			return nil
		})
	}), nil
}

func (runtime *Runtime) prepareRotate(ctx context.Context, arguments []string) (Prepared, error) {
	credentialID, source, err := parseCredentialAndFile(arguments)
	if err != nil {
		return Prepared{}, fmt.Errorf("keys rotate: %w", err)
	}
	if err := runtime.requireInitialized(); err != nil {
		return Prepared{}, err
	}
	expectedScope, expectedHead, err := runtime.singleHead(ctx, credentialID)
	if err != nil {
		return Prepared{}, fmt.Errorf("credential %q: %w", credentialID, err)
	}
	if expectedHead.State != "active" {
		return Prepared{}, errors.New("rotate cannot resurrect a revoked/deleted credential; add a new credential")
	}
	if source != "" {
		source, err = runtime.validateImportPath(source)
		if err != nil {
			return Prepared{}, err
		}
	}
	return runtime.mutation("keys.rotate", true, []string{
		fmt.Sprintf("rotate credential %q", credentialID),
		"read the replacement only after confirmation and publish a signed successor",
	}, func(ctx context.Context) error {
		return runtime.withLock(ctx, func() error {
			scope, head, err := runtime.singleHead(ctx, credentialID)
			if err != nil {
				return fmt.Errorf("credential %q has multiple heads; use resolve --rotate", credentialID)
			}
			if scope != expectedScope || head.RevisionID != expectedHead.RevisionID || head.State != "active" {
				return errors.New("credential head changed while awaiting confirmation")
			}
			payload, err := runtime.capturePayload(source)
			if err != nil {
				return err
			}
			defer clear(payload)
			if err := runtime.rejectProductionPayload(payload); err != nil {
				return err
			}
			spec := specFromMetadata(head)
			spec.State, spec.Parents = "active", []string{head.RevisionID}
			if _, err := runtime.publish(ctx, scope, spec, payload); err != nil {
				return err
			}
			fmt.Fprintf(runtime.config.Stderr, "  [ ok ] rotated credential %q\n", credentialID)
			return nil
		})
	}), nil
}

func (runtime *Runtime) prepareRollback(ctx context.Context, arguments []string) (Prepared, error) {
	values := withoutYes(arguments)
	if len(values) != 2 || !validCredentialID(values[0]) || !domain.SafeID(values[1]) {
		return Prepared{}, errors.New("keys rollback needs a credential ID and revision ID")
	}
	credentialID, revisionID := values[0], values[1]
	if err := runtime.requireInitialized(); err != nil {
		return Prepared{}, err
	}
	expectedScope, expectedHead, err := runtime.singleHead(ctx, credentialID)
	if err != nil {
		return Prepared{}, err
	}
	if expectedHead.State != "active" {
		return Prepared{}, errors.New("rollback cannot resurrect a revoked/deleted credential; add a new credential")
	}
	targetPath := runtime.recordPath(expectedScope, credentialID, revisionID)
	expectedTarget, err := runtime.readRecordMetadata(targetPath)
	if err != nil || expectedTarget.CredentialID != credentialID {
		return Prepared{}, fmt.Errorf("unknown revision %q", revisionID)
	}
	return runtime.mutation("keys.rollback", true, []string{
		fmt.Sprintf("roll back credential %q to revision %s", credentialID, revisionID),
		"decrypt the historical value and publish it as a new immutable successor",
	}, func(ctx context.Context) error {
		return runtime.withLock(ctx, func() error {
			scope, head, err := runtime.singleHead(ctx, credentialID)
			if err != nil {
				return err
			}
			if scope != expectedScope || head.RevisionID != expectedHead.RevisionID || head.State != "active" {
				return errors.New("credential head changed while awaiting confirmation")
			}
			target, err := runtime.readRecordMetadata(targetPath)
			if err != nil || target.RevisionID != expectedTarget.RevisionID || target.CredentialID != credentialID {
				return errors.New("rollback revision changed while awaiting confirmation")
			}
			payload, err := runtime.decrypt(ctx, scope, target)
			if err != nil {
				return errors.New("historical revision cannot be decrypted")
			}
			defer clear(payload)
			if err := runtime.rejectProductionPayload(payload); err != nil {
				return err
			}
			spec := specFromMetadata(head)
			spec.State, spec.Parents = "active", []string{head.RevisionID}
			if _, err := runtime.publish(ctx, scope, spec, payload); err != nil {
				return err
			}
			fmt.Fprintf(runtime.config.Stderr, "  [ ok ] rolled back credential %q to %s\n", credentialID, revisionID)
			return nil
		})
	}), nil
}

func (runtime *Runtime) prepareTerminal(
	ctx context.Context,
	action string,
	arguments []string,
) (Prepared, error) {
	credentialID, err := requiredValue(arguments, action+" needs a credential ID")
	if err != nil || !validCredentialID(credentialID) {
		return Prepared{}, errors.New(action + " needs a valid credential ID")
	}
	if err := runtime.requireInitialized(); err != nil {
		return Prepared{}, err
	}
	state := "revoked"
	if action == "delete" {
		state = "tombstone"
	}
	expectedScope, _, expectedHeads, err := runtime.credentialHeads(ctx, credentialID)
	if err != nil || len(expectedHeads) == 0 {
		return Prepared{}, firstError(err, errors.New("credential has no revisions"))
	}
	expectedHeadIDs := headIDs(expectedHeads)
	changed := len(expectedHeads) != 1 || expectedHeads[0].State != state
	actionID := domain.ActionID("keys.revoke")
	if action == "delete" {
		actionID = "keys.delete-tombstone"
	}
	return runtime.mutation(actionID, changed, []string{
		fmt.Sprintf("%s credential %q", action, credentialID),
		fmt.Sprintf("publish a %s revision and remove its materialized consumer", state),
	}, func(ctx context.Context) error {
		if !changed {
			return nil
		}
		return runtime.withLock(ctx, func() error {
			scope, revisions, heads, err := runtime.credentialHeads(ctx, credentialID)
			if err != nil || len(heads) == 0 {
				return firstError(err, errors.New("credential has no revisions"))
			}
			if scope != expectedScope || !slices.Equal(headIDs(heads), expectedHeadIDs) {
				return errors.New("credential heads changed while awaiting confirmation")
			}
			_ = revisions
			recipients := credential.RecipientIntersection(heads)
			if len(recipients) == 0 {
				identity, err := runtime.Identity()
				if err != nil {
					return err
				}
				recipients = []string{identity.ActorID}
			}
			spec := specFromMetadata(heads[0])
			spec.State, spec.Parents, spec.RecipientActors = state, headIDs(heads), recipients
			if _, err := runtime.publish(ctx, scope, spec, nil); err != nil {
				return err
			}
			if err := runtime.materializeCredential(ctx, scope, credentialID, true); err != nil {
				return err
			}
			fmt.Fprintf(runtime.config.Stderr, "  [ ok ] credential %q is %s\n", credentialID, state)
			return nil
		})
	}), nil
}

func (runtime *Runtime) prepareResolve(ctx context.Context, arguments []string) (Prepared, error) {
	values := withoutYes(arguments)
	if len(values) < 2 || !validCredentialID(values[0]) {
		return Prepared{}, errors.New("keys resolve needs a credential ID and --choose REV or --rotate")
	}
	credentialID := values[0]
	mode, chosen, source := "", "", ""
	for index := 1; index < len(values); index++ {
		switch values[index] {
		case "--choose":
			index++
			if index >= len(values) {
				return Prepared{}, errors.New("--choose needs a revision")
			}
			mode, chosen = "choose", values[index]
		case "--rotate":
			mode = "rotate"
		case "--file":
			index++
			if index >= len(values) {
				return Prepared{}, errors.New("--file needs a path")
			}
			source = values[index]
		default:
			return Prepared{}, fmt.Errorf("unexpected argument %q", values[index])
		}
	}
	if mode == "" || mode == "choose" && !domain.SafeID(chosen) {
		return Prepared{}, errors.New("resolve needs --choose REV or --rotate")
	}
	if err := runtime.requireInitialized(); err != nil {
		return Prepared{}, err
	}
	expectedScope, _, expectedHeads, err := runtime.credentialHeads(ctx, credentialID)
	if err != nil {
		return Prepared{}, err
	}
	if len(expectedHeads) <= 1 {
		return Prepared{}, fmt.Errorf("credential %q has no unresolved multi-head", credentialID)
	}
	recipients := credential.RecipientIntersection(expectedHeads)
	if len(recipients) == 0 {
		return Prepared{}, errors.New("heads have no common authorized recipient")
	}
	var expectedChosen domain.CredentialMetadata
	if mode == "choose" {
		expectedChosen, err = runtime.readRecordMetadata(runtime.recordPath(expectedScope, credentialID, chosen))
		if err != nil || expectedChosen.CredentialID != credentialID {
			return Prepared{}, fmt.Errorf("unknown revision %q", chosen)
		}
	}
	if mode == "rotate" && source != "" {
		source, err = runtime.validateImportPath(source)
		if err != nil {
			return Prepared{}, err
		}
	}
	expectedHeadIDs := headIDs(expectedHeads)
	actionID := domain.ActionID("keys.resolve-choose")
	if mode == "rotate" {
		actionID = "keys.resolve-rotate"
	}
	return runtime.mutation(actionID, true, []string{
		fmt.Sprintf("resolve all heads of credential %q", credentialID),
		map[string]string{"choose": "publish the chosen encrypted value as one successor", "rotate": "read an explicit replacement after confirmation"}[mode],
	}, func(ctx context.Context) error {
		return runtime.withLock(ctx, func() error {
			scope, _, heads, err := runtime.credentialHeads(ctx, credentialID)
			if err != nil {
				return err
			}
			if scope != expectedScope || !slices.Equal(headIDs(heads), expectedHeadIDs) {
				return errors.New("credential heads changed while awaiting confirmation")
			}
			template := heads[0]
			var payload []byte
			if mode == "choose" {
				chosenMetadata, err := runtime.readRecordMetadata(runtime.recordPath(scope, credentialID, chosen))
				if err != nil || chosenMetadata.RevisionID != expectedChosen.RevisionID {
					return errors.New("chosen revision changed while awaiting confirmation")
				}
				template = chosenMetadata
				payload, err = runtime.decrypt(ctx, scope, chosenMetadata)
				if err != nil {
					return errors.New("chosen revision cannot be decrypted")
				}
			} else {
				payload, err = runtime.capturePayload(source)
				if err != nil {
					return err
				}
				if err := runtime.rejectProductionPayload(payload); err != nil {
					clear(payload)
					return err
				}
			}
			defer clear(payload)
			spec := specFromMetadata(template)
			spec.State, spec.Parents, spec.RecipientActors = "active", headIDs(heads), recipients
			if _, err := runtime.publish(ctx, scope, spec, payload); err != nil {
				return err
			}
			fmt.Fprintf(runtime.config.Stderr, "  [ ok ] resolved credential %q\n", credentialID)
			return nil
		})
	}), nil
}

func (runtime *Runtime) prepareMaterialize(ctx context.Context, arguments []string) (Prepared, error) {
	values := withoutYes(arguments)
	zone := ""
	if len(values) > 1 {
		return Prepared{}, errors.New("materialize accepts one zone or --all")
	}
	if len(values) == 1 && values[0] != "--all" {
		if !validZone(values[0]) {
			return Prepared{}, fmt.Errorf("invalid zone %q", values[0])
		}
		zone = values[0]
	}
	if err := runtime.requireInitialized(); err != nil {
		return Prepared{}, err
	}
	changed, err := runtime.materializationChanged(ctx, zone)
	if err != nil {
		return Prepared{}, err
	}
	return runtime.mutation("keys.materialize", changed, []string{
		"materialize authorized active credential heads",
		"atomically replace only verified mode-0600 consumer files",
	}, func(ctx context.Context) error {
		if !changed {
			return nil
		}
		return runtime.withLock(ctx, func() error { return runtime.materializeAll(ctx, zone, false) })
	}), nil
}

func (runtime *Runtime) materializationChanged(ctx context.Context, zone string) (bool, error) {
	all, err := runtime.allRecords(ctx)
	if err != nil {
		return false, err
	}
	var identity *Identity
	changed := false
	for _, scope := range []ledgerScope{sharedLedger, localLedger} {
		for _, credentialID := range credentialIDs(all[scope]) {
			heads := credential.Heads(all[scope], credentialID)
			if len(heads) != 1 {
				return false, fmt.Errorf("%s has %d heads; resolve before materializing", credentialID, len(heads))
			}
			head := heads[0]
			if zone != "" && head.Zone != zone {
				continue
			}
			destination, mapped, err := runtime.consumerPath(head.Consumer, head.Zone)
			if err != nil {
				return false, err
			}
			if !mapped {
				continue
			}
			if head.State != "active" {
				if _, err := os.Lstat(destination); err == nil {
					changed = true
				} else if !errors.Is(err, os.ErrNotExist) {
					return false, err
				}
				continue
			}
			if identity == nil {
				value, err := runtime.Identity()
				if err != nil {
					return false, err
				}
				identity = &value
			}
			if contains(head.RecipientActors, identity.ActorID) {
				// Equality requires decryption, so active mapped consumers are
				// conservatively assessed as changed without opening plaintext.
				changed = true
			}
		}
	}
	return changed, nil
}

func (runtime *Runtime) prepareSync(ctx context.Context, arguments []string) (Prepared, error) {
	values := withoutYes(arguments)
	target := ""
	for _, value := range values {
		switch {
		case value == "--all" || value == "--now":
		case strings.HasPrefix(value, "@") && domain.SafeName(strings.TrimPrefix(value, "@")):
			if target != "" {
				return Prepared{}, errors.New("sync accepts at most one peer")
			}
			target = strings.TrimPrefix(value, "@")
		default:
			return Prepared{}, fmt.Errorf("keys sync: unexpected %q", value)
		}
	}
	if err := runtime.requireInitialized(); err != nil {
		return Prepared{}, err
	}
	selected := []domain.CredentialPeer{}
	if target != "" {
		peer, err := runtime.peer(target)
		if err != nil {
			return Prepared{}, err
		}
		role, err := peerRole(peer)
		if err != nil {
			return Prepared{}, err
		}
		if role != "active" {
			return Prepared{}, fmt.Errorf("peer %q is passive (respond-only); register a reverse route first", target)
		}
		selected = append(selected, peer)
	} else {
		peers, err := runtime.peers()
		if err != nil {
			return Prepared{}, err
		}
		for _, peer := range peers {
			role, err := peerRole(peer)
			if err != nil {
				return Prepared{}, err
			}
			if role == "active" {
				selected = append(selected, peer)
			}
		}
	}
	expectedHeads, err := runtime.headSnapshot(ctx)
	if err != nil {
		return Prepared{}, err
	}
	return runtime.mutation("keys.sync", len(selected) != 0, []string{
		map[bool]string{true: "synchronize the signed credential ledger with " + target, false: "synchronize all active credential peers"}[target != ""],
		"verify append-only history, signatures, ciphertext recipients and DAG policy before merge",
	}, func(ctx context.Context) error {
		if len(selected) == 0 {
			return nil
		}
		return runtime.withLock(ctx, func() error {
			if err := runtime.requireInitialized(); err != nil {
				return err
			}
			currentHeads, err := runtime.headSnapshot(ctx)
			if err != nil || !slices.Equal(currentHeads, expectedHeads) {
				return fmt.Errorf("%w: credential heads changed while awaiting confirmation", domain.ErrPlanStale)
			}
			var failures []error
			for _, expected := range selected {
				peer, err := runtime.peer(expected.Name)
				if err != nil {
					return fmt.Errorf("%w: credential peer %q changed while awaiting confirmation", domain.ErrPlanStale, expected.Name)
				}
				if peer != expected {
					return fmt.Errorf("%w: credential peer %q changed while awaiting confirmation", domain.ErrPlanStale, expected.Name)
				}
				if err := runtime.syncPreparedPeer(ctx, expected); err != nil {
					failures = append(failures, err)
				}
			}
			return errors.Join(failures...)
		})
	}), nil
}

func (runtime *Runtime) prepareAutoSync(arguments []string) (Prepared, error) {
	values := withoutYes(arguments)
	action := "status"
	if len(values) != 0 {
		action, values = values[0], values[1:]
	}
	if action == "status" {
		if len(values) != 0 {
			return Prepared{}, errors.New("auto-sync status takes no target")
		}
		return runtime.readOperation("keys.auto-sync-status", runtime.status), nil
	}
	if action != "pause" && action != "resume" {
		return Prepared{}, errors.New("auto-sync expects status, pause or resume")
	}
	if err := runtime.requireInitialized(); err != nil {
		return Prepared{}, err
	}
	target := ""
	for _, value := range values {
		if value == "--all" {
			continue
		}
		if !strings.HasPrefix(value, "@") || !domain.SafeName(strings.TrimPrefix(value, "@")) || target != "" {
			return Prepared{}, fmt.Errorf("auto-sync: unexpected %q", value)
		}
		target = strings.TrimPrefix(value, "@")
	}
	peers, err := runtime.peers()
	if err != nil {
		return Prepared{}, err
	}
	selected := make([]domain.CredentialPeer, 0, len(peers))
	matched := target == ""
	desiredManual := action == "pause"
	changed := false
	for _, peer := range peers {
		if target != "" && peer.Name != target {
			continue
		}
		matched = true
		role, err := peerRole(peer)
		if err != nil {
			return Prepared{}, err
		}
		if role != "active" {
			if target != "" {
				return Prepared{}, fmt.Errorf("peer %q is passive (respond-only); register a reverse route first", target)
			}
			continue
		}
		selected = append(selected, peer)
		changed = changed || peer.ManualOnly != desiredManual
	}
	if !matched {
		return Prepared{}, fmt.Errorf("credential peer %q is not enrolled", target)
	}
	return runtime.mutation(domain.ActionID("keys.auto-sync-"+action), changed, []string{
		fmt.Sprintf("%s automatic credential synchronization", action),
		"update only active outbound peer policy; passive peers remain respond-only",
	}, func(ctx context.Context) error {
		if !changed {
			return nil
		}
		return runtime.withLock(ctx, func() error {
			for _, expected := range selected {
				peer, err := runtime.peer(expected.Name)
				if err != nil {
					return err
				}
				role, err := peerRole(peer)
				if err != nil {
					return err
				}
				if role != "active" || peer.ManualOnly != expected.ManualOnly {
					return fmt.Errorf("peer %q policy changed while awaiting confirmation", peer.Name)
				}
				peer.ManualOnly = desiredManual
				payload, err := json.MarshalIndent(peer, "", "  ")
				if err != nil {
					return err
				}
				path, _ := runtime.peerPath(peer.Name)
				if err := atomicWrite(path, append(payload, '\n'), 0o600); err != nil {
					return err
				}
			}
			fmt.Fprintf(runtime.config.Stderr, "  [ ok ] automatic credential sync %sd\n", action)
			return nil
		})
	}), nil
}

func (runtime *Runtime) prepareTrust(ctx context.Context, arguments []string) (Prepared, error) {
	values := withoutYes(arguments)
	if len(values) == 0 || !strings.HasPrefix(values[0], "@") {
		return Prepared{}, errors.New("keys trust needs @peer")
	}
	name := strings.TrimPrefix(values[0], "@")
	if !domain.SafeName(name) {
		return Prepared{}, errors.New("invalid credential peer")
	}
	manual := false
	for _, value := range values[1:] {
		if value != "--manual-only" {
			return Prepared{}, fmt.Errorf("trust: unexpected %q", value)
		}
		manual = true
	}
	if err := runtime.requireInitialized(); err != nil {
		return Prepared{}, err
	}
	target, err := runtime.resolveTarget(ctx, name)
	if err != nil {
		return Prepared{}, err
	}
	identityBytes, err := runtime.callTarget(ctx, target, []string{"_keys-exchange", "identity"}, nil)
	if err != nil {
		return Prepared{}, fmt.Errorf("peer %q has no initialized credential ledger: %w", name, err)
	}
	identity, err := decodeIdentity(identityBytes)
	if err != nil {
		return Prepared{}, fmt.Errorf("peer %q returned invalid identity data", name)
	}
	local, err := runtime.Identity()
	if err != nil {
		return Prepared{}, err
	}
	if identity.ActorID == local.ActorID {
		return Prepared{}, errors.New("peer resolves to this same key identity")
	}
	fingerprint, err := signingFingerprint(identity.SigningPublic)
	if err != nil {
		return Prepared{}, err
	}
	expectedPeers, err := runtime.peers()
	if err != nil {
		return Prepared{}, err
	}
	expectedHeads, err := runtime.headSnapshot(ctx)
	if err != nil {
		return Prepared{}, err
	}
	return runtime.mutation("keys.trust", true, []string{
		fmt.Sprintf("trust credential peer %q", name),
		fmt.Sprintf("actor=%s age=%s signing=%s", identity.ActorID, identity.AgeRecipient, fingerprint),
		"exchange public identities and re-encrypt current shared heads for the new recipient",
		map[bool]string{true: "keep synchronization manual", false: "enable unattended encrypted synchronization"}[manual],
	}, func(ctx context.Context) error {
		currentTarget, err := runtime.resolveTarget(ctx, name)
		if err != nil || currentTarget != target {
			return fmt.Errorf("%w: credential peer %q destination changed while awaiting confirmation", domain.ErrPlanStale, name)
		}
		currentIdentityBytes, err := runtime.callTarget(ctx, target, []string{"_keys-exchange", "identity"}, nil)
		if err != nil {
			return fmt.Errorf("recheck credential peer %q identity: %w", name, err)
		}
		currentIdentity, err := decodeIdentity(currentIdentityBytes)
		if err != nil || currentIdentity != identity {
			return fmt.Errorf("%w: credential peer %q identity changed while awaiting confirmation", domain.ErrPlanStale, name)
		}
		return runtime.withLock(ctx, func() error {
			currentPeers, err := runtime.peers()
			if err != nil || !slices.Equal(currentPeers, expectedPeers) {
				return fmt.Errorf("%w: credential peer policy changed while awaiting confirmation", domain.ErrPlanStale)
			}
			currentHeads, err := runtime.headSnapshot(ctx)
			if err != nil || !slices.Equal(currentHeads, expectedHeads) {
				return fmt.Errorf("%w: credential heads changed while awaiting confirmation", domain.ErrPlanStale)
			}
			peer, err := runtime.storePeer(name, identity, target.Transport, target.Destination, target.RemoteYard, manual)
			if err != nil {
				return fmt.Errorf("peer trust metadata was rejected: %w", err)
			}
			localPayload, err := json.Marshal(local)
			if err != nil {
				return err
			}
			if _, err := runtime.callTarget(ctx, target,
				[]string{"_keys-exchange", "trust-import", runtime.config.Context}, bytes.NewReader(localPayload)); err != nil {
				return fmt.Errorf("peer %q did not accept reciprocal trust; local enrollment is kept", name)
			}
			if err := runtime.rekeyShared(ctx, identity.ActorID, true); err != nil {
				return err
			}
			if err := runtime.syncPreparedPeer(ctx, peer); err != nil {
				return err
			}
			fmt.Fprintf(runtime.config.Stderr, "  [ ok ] trusted credential peer %q\n", name)
			return nil
		})
	}), nil
}

func (runtime *Runtime) prepareUntrust(arguments []string) (Prepared, error) {
	value, err := requiredValue(arguments, "keys untrust needs @peer")
	if err != nil || !strings.HasPrefix(value, "@") || !domain.SafeName(strings.TrimPrefix(value, "@")) {
		return Prepared{}, errors.New("keys untrust needs @peer")
	}
	name := strings.TrimPrefix(value, "@")
	if err := runtime.requireInitialized(); err != nil {
		return Prepared{}, err
	}
	peer, err := runtime.peer(name)
	if err != nil {
		return Prepared{}, err
	}
	return runtime.mutation("keys.untrust", true, []string{
		fmt.Sprintf("remove credential peer %q", name),
		"publish successor revisions without that recipient and remove signing trust",
		"plaintext or ciphertext already received cannot be erased; rotate upstream values separately",
	}, func(ctx context.Context) error {
		return runtime.withLock(ctx, func() error {
			current, err := runtime.peer(name)
			if err != nil {
				return fmt.Errorf("credential peer %q changed while awaiting confirmation: %w", name, err)
			}
			if current != peer {
				return fmt.Errorf("credential peer %q changed while awaiting confirmation", name)
			}
			if err := runtime.rekeyShared(ctx, peer.ActorID, false); err != nil {
				return err
			}
			identity, err := runtime.Identity()
			if err != nil {
				return err
			}
			if _, err := runtime.callPeer(ctx, peer, []string{"_keys-exchange", "untrust-import", identity.ActorID}, nil); err != nil {
				fmt.Fprintf(runtime.config.Stderr, "  [warn] peer %q was unreachable; reciprocal trust remains\n", name)
			}
			path, _ := runtime.peerPath(name)
			state, _ := runtime.statePath(name)
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			_ = os.Remove(state)
			if err := runtime.rebuildAllowedSigners(); err != nil {
				return err
			}
			fmt.Fprintf(runtime.config.Stderr, "  [ ok ] removed credential peer %q\n", name)
			return nil
		})
	}), nil
}

type movePlan struct {
	credentialID, targetName, targetActor, targetContext, targetAssignment string
	target                                                                 *Target
	expectedPeer                                                           *domain.CredentialPeer
	expectedHead                                                           domain.CredentialMetadata
	current, zone, expectedRevision                                        string
	expectedEpoch, nextEpoch                                               int64
	resume                                                                 bool
}

func (runtime *Runtime) prepareMove(ctx context.Context, arguments []string) (Prepared, error) {
	values := withoutYes(arguments)
	if len(values) != 2 || !validCredentialID(values[0]) || !strings.HasPrefix(values[1], "@") {
		return Prepared{}, errors.New("keys move needs a credential ID and @peer")
	}
	if err := runtime.requireInitialized(); err != nil {
		return Prepared{}, err
	}
	plan, err := runtime.planMove(ctx, values[0], strings.TrimPrefix(values[1], "@"))
	if err != nil {
		return Prepared{}, err
	}
	consequences := []string{
		fmt.Sprintf("move exclusive credential %q to %s", plan.credentialID, plan.targetAssignment),
	}
	if plan.resume {
		consequences = append(consequences, "resume ciphertext sync and target materialization without publishing another epoch")
	} else {
		consequences = append(consequences,
			fmt.Sprintf("stop and verify the old staging consumer for zone %q", plan.zone),
			fmt.Sprintf("publish authority assignment epoch %d", plan.nextEpoch),
			"sync and materialize on the target before reporting success")
	}
	return runtime.mutation("keys.move", true, consequences, func(ctx context.Context) error { return runtime.executeMove(ctx, plan) }), nil
}

func (runtime *Runtime) planMove(ctx context.Context, credentialID, targetName string) (movePlan, error) {
	if !domain.SafeName(targetName) {
		return movePlan{}, errors.New("invalid move target")
	}
	scope, head, err := runtime.singleHead(ctx, credentialID)
	if err != nil {
		return movePlan{}, err
	}
	if scope != sharedLedger {
		return movePlan{}, errors.New("local-only credentials cannot move to a peer")
	}
	identity, err := runtime.Identity()
	if err != nil {
		return movePlan{}, err
	}
	if !head.Exclusive {
		return movePlan{}, fmt.Errorf("credential %q is not exclusive", credentialID)
	}
	if head.AuthorityHost != identity.ActorID {
		return movePlan{}, errors.New("only the immutable authority host may move this credential")
	}
	plan := movePlan{
		credentialID: credentialID, targetName: targetName, current: head.AssignedYard,
		zone: head.Zone, expectedRevision: head.RevisionID, expectedEpoch: head.AssignmentEpoch,
		expectedHead: head,
	}
	if peer, err := runtime.peer(targetName); err == nil {
		assignment, err := peerAssignment(peer)
		if err != nil {
			return movePlan{}, err
		}
		plan.targetActor, plan.targetAssignment = peer.ActorID, assignment
		plan.targetContext = strings.SplitN(assignment, "/", 2)[1]
		target := Target{Name: peer.Name, Transport: peer.Transport, Destination: peer.Dest, RemoteYard: peer.RemoteYard}
		plan.target, plan.expectedPeer = &target, &peer
	} else if targetName == runtime.config.Context {
		plan.targetActor, plan.targetContext = identity.ActorID, runtime.config.Context
		plan.targetAssignment = identity.ActorID + "/" + runtime.config.Context
		target := Target{Name: targetName, Transport: "local"}
		plan.target = &target
	} else {
		target, err := runtime.resolveTarget(ctx, targetName)
		if err != nil {
			return movePlan{}, err
		}
		plan.target = &target
		if target.Transport == "local" {
			plan.targetActor, plan.targetContext = identity.ActorID, targetName
		} else {
			identityBytes, err := runtime.callTarget(ctx, target, []string{"_keys-exchange", "identity"}, nil)
			if err != nil {
				return movePlan{}, fmt.Errorf("target %q has no initialized credential ledger", targetName)
			}
			targetIdentity, err := decodeIdentity(identityBytes)
			if err != nil {
				return movePlan{}, err
			}
			plan.targetActor = targetIdentity.ActorID
			plan.targetContext = firstNonEmpty(target.RemoteYard, "default")
		}
		plan.targetAssignment = plan.targetActor + "/" + plan.targetContext
	}
	if plan.targetActor != identity.ActorID {
		peer, found, err := runtime.peerByActor(plan.targetActor)
		if err != nil || !found {
			return movePlan{}, firstError(err, fmt.Errorf("target host %q is not trusted", targetName))
		}
		role, err := peerRole(peer)
		if err != nil || role != "active" {
			return movePlan{}, errors.New("target host has no active return route")
		}
		if !contains(head.RecipientActors, plan.targetActor) {
			return movePlan{}, errors.New("target host is not an encrypted recipient")
		}
		plan.expectedPeer = &peer
	}
	plan.resume = plan.current == plan.targetAssignment
	if !plan.resume {
		moved, err := credential.MoveAssignment(head, identity.ActorID, plan.targetAssignment, head.AssignmentEpoch)
		if err != nil {
			return movePlan{}, err
		}
		plan.nextEpoch = moved.AssignmentEpoch
	}
	return plan, nil
}

func (runtime *Runtime) executeMove(ctx context.Context, plan movePlan) error {
	if plan.resume {
		if err := runtime.withLock(ctx, func() error {
			scope, head, err := runtime.singleHead(ctx, plan.credentialID)
			if err != nil || scope != sharedLedger || !sameCredentialMetadata(head, plan.expectedHead) ||
				head.RevisionID != plan.expectedRevision ||
				head.AssignmentEpoch != plan.expectedEpoch || head.AssignedYard != plan.current {
				return fmt.Errorf("%w: exclusive assignment changed while awaiting confirmation", domain.ErrPlanStale)
			}
			if plan.expectedPeer != nil {
				currentPeer, err := runtime.peer(plan.expectedPeer.Name)
				if err != nil || currentPeer != *plan.expectedPeer {
					return fmt.Errorf("%w: move target peer changed while awaiting confirmation", domain.ErrPlanStale)
				}
			}
			return runtime.materializeCredential(ctx, sharedLedger, plan.credentialID, true)
		}); err != nil {
			return err
		}
		if plan.expectedPeer != nil {
			if err := runtime.syncPreparedPeer(ctx, *plan.expectedPeer); err != nil {
				return err
			}
		}
		if plan.targetAssignment != runtime.currentYardID() {
			if _, err := runtime.callTarget(ctx, *plan.target, []string{"_keys-exchange", "refresh", runtime.actorID()}, nil); err != nil {
				return err
			}
		}
		fmt.Fprintf(runtime.config.Stderr, "  [ ok ] exclusive credential %q is assigned and synchronized\n", plan.credentialID)
		return nil
	}
	if err := runtime.withLock(ctx, func() error {
		scope, head, err := runtime.singleHead(ctx, plan.credentialID)
		if err != nil {
			return err
		}
		if scope != sharedLedger || head.RevisionID != plan.expectedRevision ||
			head.AssignmentEpoch != plan.expectedEpoch || head.AssignedYard != plan.current {
			return errors.New("exclusive assignment changed while awaiting confirmation")
		}
		if err := runtime.stopAssignedConsumer(ctx, plan.current, plan.zone); err != nil {
			return errors.New("old assigned yard is unreachable or could not confirm stop; handoff aborted")
		}
		payload, err := runtime.decrypt(ctx, sharedLedger, head)
		if err != nil {
			return errors.New("current credential payload cannot be decrypted")
		}
		defer clear(payload)
		spec := specFromMetadata(head)
		spec.State, spec.Parents = "active", []string{head.RevisionID}
		spec.AssignedYard, spec.AssignmentEpoch = plan.targetAssignment, plan.nextEpoch
		if _, err := runtime.publish(ctx, sharedLedger, spec, payload); err != nil {
			return err
		}
		return runtime.materializeCredential(ctx, sharedLedger, plan.credentialID, true)
	}); err != nil {
		return err
	}
	if err := runtime.syncAssignmentHost(ctx, plan.current); err != nil {
		return errors.New("handoff was published, but the old assigned host did not synchronize")
	}
	currentActor, _, _ := splitAssignment(plan.current)
	if plan.expectedPeer != nil && currentActor != plan.targetActor {
		if err := runtime.syncPreparedPeer(ctx, *plan.expectedPeer); err != nil {
			return errors.New("handoff was published, but the target host did not synchronize")
		}
	}
	if plan.current != runtime.currentYardID() {
		if err := runtime.refreshAssignment(ctx, plan.current); err != nil {
			return errors.New("handoff was published, but the old assigned yard did not refresh")
		}
	}
	if plan.targetAssignment != runtime.currentYardID() {
		if _, err := runtime.callTarget(ctx, *plan.target, []string{"_keys-exchange", "refresh", runtime.actorID()}, nil); err != nil {
			return errors.New("handoff was published, but the target yard did not materialize")
		}
	}
	fmt.Fprintf(runtime.config.Stderr, "  [ ok ] moved exclusive credential %q to %s (epoch %d)\n",
		plan.credentialID, plan.targetAssignment, plan.nextEpoch)
	return nil
}

func (runtime *Runtime) prepareExchange(arguments []string) (Prepared, error) {
	if len(arguments) == 0 {
		return Prepared{}, errors.New("_keys-exchange needs an action")
	}
	action, arguments := arguments[0], arguments[1:]
	switch action {
	case "identity":
		if len(arguments) != 0 {
			return Prepared{}, errors.New("identity takes no arguments")
		}
		if err := runtime.requireInitialized(); err != nil {
			return Prepared{}, err
		}
		return runtime.readOperation("keys.exchange.identity", func(context.Context) error {
			identity, err := runtime.Identity()
			if err != nil {
				return err
			}
			return json.NewEncoder(runtime.config.Stdout).Encode(identity)
		}), nil
	case "bare-path":
		if len(arguments) != 0 {
			return Prepared{}, errors.New("bare-path takes no arguments")
		}
		if err := runtime.requireInitialized(); err != nil {
			return Prepared{}, err
		}
		return runtime.readOperation("keys.exchange.bare-path", func(context.Context) error {
			fmt.Fprintln(runtime.config.Stdout, runtime.sharedBare)
			return nil
		}), nil
	case "trust-import":
		if len(arguments) != 1 || !domain.SafeName(arguments[0]) {
			return Prepared{}, errors.New("trust-import needs a peer name")
		}
		name := arguments[0]
		if err := runtime.requireInitialized(); err != nil {
			return Prepared{}, err
		}
		return runtime.mutation("keys.exchange.trust-import", true, []string{"accept reciprocal credential trust"}, func(ctx context.Context) error {
			identityBytes, err := io.ReadAll(io.LimitReader(runtime.config.Stdin, maximumOutput+1))
			if err != nil || len(identityBytes) > maximumOutput {
				return errors.New("missing or oversized peer identity")
			}
			identity, err := decodeIdentity(identityBytes)
			if err != nil {
				return err
			}
			return runtime.withLock(ctx, func() error {
				if _, err := runtime.storePeer(name, identity, "inbound", "", "", true); err != nil {
					return err
				}
				if err := runtime.rekeyShared(ctx, identity.ActorID, true); err != nil {
					return err
				}
				fmt.Fprintf(runtime.config.Stderr, "  [ ok ] accepted reciprocal key trust for %q\n", name)
				return nil
			})
		}), nil
	case "untrust-import":
		if len(arguments) != 1 || !domain.SafeID(arguments[0]) {
			return Prepared{}, errors.New("untrust-import needs an actor ID")
		}
		actor := arguments[0]
		if err := runtime.requireInitialized(); err != nil {
			return Prepared{}, err
		}
		expectedPeer, found, err := runtime.peerByActor(actor)
		if err != nil {
			return Prepared{}, err
		}
		return runtime.mutation("keys.exchange.untrust-import", found, []string{"remove reciprocal credential trust"}, func(ctx context.Context) error {
			if !found {
				return nil
			}
			return runtime.withLock(ctx, func() error {
				peer, found, err := runtime.peerByActor(actor)
				if err != nil {
					return fmt.Errorf("credential peer changed while awaiting confirmation: %w", err)
				}
				if !found || peer != expectedPeer {
					return errors.New("credential peer changed while awaiting confirmation")
				}
				if err := runtime.rekeyShared(ctx, actor, false); err != nil {
					return err
				}
				path, _ := runtime.peerPath(peer.Name)
				state, _ := runtime.statePath(peer.Name)
				_ = os.Remove(path)
				_ = os.Remove(state)
				return runtime.rebuildAllowedSigners()
			})
		}), nil
	case "refresh":
		if len(arguments) > 1 || len(arguments) == 1 && !domain.SafeID(arguments[0]) {
			return Prepared{}, errors.New("refresh accepts one actor ID")
		}
		if err := runtime.requireInitialized(); err != nil {
			return Prepared{}, err
		}
		sourceActor := ""
		if len(arguments) == 1 {
			sourceActor = arguments[0]
		}
		return runtime.mutation("keys.exchange.refresh", true, []string{"refresh encrypted credential heads and materialization"}, func(ctx context.Context) error {
			return runtime.withLock(ctx, func() error {
				if sourceActor != "" {
					if peer, found, err := runtime.peerByActor(sourceActor); err != nil {
						return err
					} else if found {
						head, err := runtime.gitRun(ctx, runtime.shared, "rev-parse", "main")
						if err != nil {
							return err
						}
						if err := runtime.writeState(peer.Name, true, "", strings.TrimSpace(string(head))); err != nil {
							return err
						}
					}
				}
				if err := runtime.refreshShared(ctx); err != nil {
					return err
				}
				_, _ = runtime.reconcileShared(ctx)
				if _, err := runtime.gitRun(ctx, runtime.shared, "push", "-q", "origin", "main"); err != nil {
					return err
				}
				_ = runtime.materializeAll(ctx, "", true)
				return nil
			})
		}), nil
	default:
		return Prepared{}, fmt.Errorf("unknown keys exchange action %q", action)
	}
}

func (runtime *Runtime) prepareAutoWorker(arguments []string) (Prepared, error) {
	values := withoutYes(arguments)
	if len(values) != 1 || values[0] != "--if-due" {
		return Prepared{}, errors.New("_keys-auto-sync expects --if-due")
	}
	changed, err := runtime.automaticSyncChanged()
	if err != nil {
		return Prepared{}, err
	}
	return runtime.mutation("keys.auto-worker", changed, []string{"synchronize due automatic credential peers"}, func(ctx context.Context) error {
		if !changed {
			return nil
		}
		return runtime.withLock(ctx, func() error { return runtime.syncAll(ctx, true) })
	}), nil
}

func (runtime *Runtime) automaticSyncChanged() (bool, error) {
	if !runtime.Initialized() {
		return false, nil
	}
	peers, err := runtime.peers()
	if err != nil {
		return false, err
	}
	now := time.Now().Unix()
	interval := time.Duration(positiveInt(runtime.env["SUBYARD_KEYS_IF_DUE_SECONDS"], 3600)) * time.Second
	for _, peer := range peers {
		role, err := peerRole(peer)
		if err != nil {
			return false, err
		}
		if role != "active" || peer.ManualOnly {
			continue
		}
		state, err := runtime.readState(peer.Name)
		if err != nil {
			return false, err
		}
		due, err := credential.SyncDue(state, now, interval)
		if err != nil {
			return false, err
		}
		if due {
			return true, nil
		}
	}
	return false, nil
}

func (runtime *Runtime) rekeyShared(ctx context.Context, actor string, trust bool) error {
	if err := runtime.refreshShared(ctx); err != nil {
		return err
	}
	revisions, err := runtime.records(ctx, sharedLedger)
	if err != nil {
		return err
	}
	credentials := credentialIDs(revisions)
	for _, credentialID := range credentials {
		heads := credential.Heads(revisions, credentialID)
		if len(heads) != 1 {
			continue
		}
		head := heads[0]
		recipients, err := credential.RekeyRecipients(head.RecipientActors, actor, trust)
		if err != nil || slices.Equal(recipients, compactSorted(append([]string(nil), head.RecipientActors...))) {
			continue
		}
		var payload []byte
		if head.State == "active" {
			payload, err = runtime.decrypt(ctx, sharedLedger, head)
			if err != nil {
				continue
			}
		}
		spec := specFromMetadata(head)
		spec.Parents, spec.RecipientActors = []string{head.RevisionID}, recipients
		_, publishErr := runtime.publish(ctx, sharedLedger, spec, payload)
		clear(payload)
		if publishErr != nil {
			return publishErr
		}
		revisions, err = runtime.records(ctx, sharedLedger)
		if err != nil {
			return err
		}
	}
	return nil
}

func (runtime *Runtime) list(ctx context.Context) error {
	if err := runtime.requireInitialized(); err != nil {
		return err
	}
	all, err := runtime.allRecords(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(runtime.config.Stdout, "ID\tPOLICY\tHEADS\tSTATE\tKIND\tZONE\tCONSUMER\tLABEL")
	for _, scope := range []ledgerScope{sharedLedger, localLedger} {
		policy := "syncable"
		if scope == localLedger {
			policy = "local-only"
		}
		for _, credentialID := range credentialIDs(all[scope]) {
			heads := credential.Heads(all[scope], credentialID)
			if len(heads) == 0 {
				continue
			}
			state := "conflict"
			if len(heads) == 1 {
				state = heads[0].State
			}
			fmt.Fprintf(runtime.config.Stdout, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
				credentialID, policy, len(heads), state, heads[0].Kind, heads[0].Zone,
				heads[0].Consumer, heads[0].Label)
		}
	}
	return nil
}

func (runtime *Runtime) status(ctx context.Context) error {
	if !runtime.Initialized() {
		fmt.Fprintln(runtime.config.Stdout, "credential ledger is not initialized (run: yard init)")
		return nil
	}
	all, err := runtime.allRecords(ctx)
	if err != nil {
		return err
	}
	conflicts := 0
	for _, revisions := range all {
		for _, credentialID := range credentialIDs(revisions) {
			if len(credential.Heads(revisions, credentialID)) > 1 {
				conflicts++
			}
		}
	}
	identity, err := runtime.Identity()
	if err != nil {
		return err
	}
	peers, err := runtime.peers()
	if err != nil {
		return err
	}
	fmt.Fprintf(runtime.config.Stdout, "keys     host=%s yard=%s root=%s peers=%d conflicts=%d\n",
		identity.ActorID, runtime.config.Context, runtime.config.Root, len(peers), conflicts)
	now := time.Now().Unix()
	for _, peer := range peers {
		role, err := peerRole(peer)
		if err != nil {
			return err
		}
		policy := "automatic"
		if role == "passive" {
			policy = "respond-only"
		} else if peer.ManualOnly {
			policy = "manual"
		}
		if role == "passive" {
			fmt.Fprintf(runtime.config.Stdout,
				"  peer %-16s role=%-7s policy=%-12s last-success=n/a last-attempt=n/a next-retry=n/a\n",
				peer.Name, role, policy)
			continue
		}
		state, err := runtime.readState(peer.Name)
		if err != nil {
			return err
		}
		lastSuccess := "never"
		if state.LastSuccess > 0 {
			lastSuccess = ageHuman(now-state.LastSuccess) + " ago"
		}
		lastAttempt := "never"
		if state.LastAttempt > 0 {
			lastAttempt = ageHuman(now - state.LastAttempt)
		}
		next := "due"
		if state.NextRetry > now {
			next = "in " + ageHuman(state.NextRetry-now)
		}
		errorText := ""
		if state.Error != "" {
			errorText = " error=" + state.Error
		}
		fmt.Fprintf(runtime.config.Stdout,
			"  peer %-16s role=%-7s policy=%-12s last-success=%s last-attempt=%s next-retry=%s%s\n",
			peer.Name, role, policy, lastSuccess, lastAttempt, next, errorText)
		if policy == "automatic" && (state.LastSuccess == 0 || now-state.LastSuccess > 86400) {
			fmt.Fprintf(runtime.config.Stderr, "  [warn] credential sync with %q is stale (>24h or never successful)\n", peer.Name)
		}
	}
	if conflicts != 0 {
		fmt.Fprintf(runtime.config.Stderr, "  [warn] %d credential(s) need explicit resolve before materialization\n", conflicts)
	}
	return nil
}

func (runtime *Runtime) history(ctx context.Context, wanted string) error {
	if wanted != "" && !validCredentialID(wanted) {
		return errors.New("history needs a valid credential ID")
	}
	if err := runtime.requireInitialized(); err != nil {
		return err
	}
	all, err := runtime.allRecords(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(runtime.config.Stdout, "CREDENTIAL\tREVISION\tACTOR\tCOUNTER\tSTATE\tPARENTS\tRECIPIENTS\tTIMESTAMP")
	for _, scope := range []ledgerScope{sharedLedger, localLedger} {
		for _, metadata := range all[scope] {
			if wanted != "" && metadata.CredentialID != wanted {
				continue
			}
			fmt.Fprintf(runtime.config.Stdout, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
				metadata.CredentialID, metadata.RevisionID, metadata.ActorID, metadata.ActorCounter,
				metadata.State, strings.Join(metadata.Parents, ","), strings.Join(metadata.RecipientActors, ","),
				metadata.Timestamp.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

func (runtime *Runtime) singleHead(ctx context.Context, credentialID string) (ledgerScope, domain.CredentialMetadata, error) {
	scope, revisions, heads, err := runtime.credentialHeads(ctx, credentialID)
	if err != nil {
		return "", domain.CredentialMetadata{}, err
	}
	_ = revisions
	if len(heads) != 1 {
		return "", domain.CredentialMetadata{}, fmt.Errorf("credential %q has %d heads", credentialID, len(heads))
	}
	return scope, heads[0], nil
}

func (runtime *Runtime) credentialHeads(
	ctx context.Context,
	credentialID string,
) (ledgerScope, []domain.CredentialMetadata, []domain.CredentialMetadata, error) {
	scope, err := runtime.findScope(credentialID)
	if err != nil {
		return "", nil, nil, err
	}
	revisions, err := runtime.records(ctx, scope)
	if err != nil {
		return "", nil, nil, err
	}
	return scope, revisions, credential.Heads(revisions, credentialID), nil
}

func (runtime *Runtime) consumerConflict(ctx context.Context, consumerName, zone string) (string, error) {
	if consumerName == "none" {
		return "", nil
	}
	all, err := runtime.allRecords(ctx)
	if err != nil {
		return "", err
	}
	for _, scope := range []ledgerScope{sharedLedger, localLedger} {
		for _, credentialID := range credentialIDs(all[scope]) {
			heads := credential.Heads(all[scope], credentialID)
			if len(heads) == 1 && heads[0].State == "active" &&
				heads[0].Consumer == consumerName && heads[0].Zone == zone {
				return credentialID, nil
			}
		}
	}
	return "", nil
}

func (runtime *Runtime) materializeAll(ctx context.Context, zone string, automatic bool) error {
	all, err := runtime.allRecords(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, scope := range []ledgerScope{sharedLedger, localLedger} {
		for _, credentialID := range credentialIDs(all[scope]) {
			heads := credential.Heads(all[scope], credentialID)
			if len(heads) != 1 {
				failures = append(failures, fmt.Errorf("%s has %d heads", credentialID, len(heads)))
				continue
			}
			if zone != "" && heads[0].Zone != zone {
				continue
			}
			if err := runtime.materializeCredential(ctx, scope, credentialID, automatic); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func (runtime *Runtime) materializeCredential(ctx context.Context, scope ledgerScope, credentialID string, automatic bool) error {
	revisions, err := runtime.records(ctx, scope)
	if err != nil {
		return err
	}
	heads := credential.Heads(revisions, credentialID)
	if len(heads) != 1 {
		return fmt.Errorf("%s has %d heads; resolve before materializing", credentialID, len(heads))
	}
	head := heads[0]
	destination, mapped, err := runtime.consumerPath(head.Consumer, head.Zone)
	if err != nil || !mapped {
		return err
	}
	if head.State != "active" {
		if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	identity, err := runtime.Identity()
	if err != nil {
		return err
	}
	if !contains(head.RecipientActors, identity.ActorID) {
		return nil
	}
	if head.Exclusive {
		trusted, lastSuccess := false, int64(0)
		if head.AuthorityHost != identity.ActorID {
			if peer, found, err := runtime.peerByActor(head.AuthorityHost); err != nil {
				return err
			} else if found {
				trusted = true
				state, err := runtime.readState(peer.Name)
				if err != nil {
					return err
				}
				lastSuccess = state.LastSuccess
			}
		}
		decision, err := credential.CheckExclusiveAccess(head, identity.ActorID, runtime.currentYardID(),
			trusted, lastSuccess, time.Now().Unix(),
			time.Duration(positiveInt(runtime.env["SUBYARD_KEYS_AUTHORITY_MAX_AGE"], 3600))*time.Second)
		if err != nil {
			return err
		}
		switch decision.Reason {
		case "authority-local", "authority-fresh":
		case "not-assigned":
			if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		case "authority-untrusted":
			return fmt.Errorf("%s has no trusted authority", credentialID)
		case "authority-stale":
			return fmt.Errorf("%s has no fresh authority exchange", credentialID)
		default:
			return fmt.Errorf("%s has invalid exclusive access", credentialID)
		}
	}
	payload, err := runtime.decrypt(ctx, scope, head)
	if err != nil {
		return err
	}
	defer clear(payload)
	if err := atomicWrite(destination, payload, 0o600); err != nil {
		return err
	}
	if !automatic {
		fmt.Fprintf(runtime.config.Stderr, "  [ ok ] materialized %s -> %s\n", credentialID, destination)
	}
	return nil
}

func (runtime *Runtime) consumerPath(consumerName, zone string) (string, bool, error) {
	if !validZone(zone) {
		return "", false, errors.New("invalid credential zone")
	}
	var destination string
	switch consumerName {
	case "none", "":
		return "", false, nil
	case "staging-env":
		destination = filepath.Join(runtime.config.ConsumerRoot, "staging", zone+".env")
	case "qa-secrets":
		destination = filepath.Join(runtime.config.ConsumerRoot, "qa-pool", "secrets.env")
	case "qa-pool":
		destination = filepath.Join(runtime.config.ConsumerRoot, "qa-pool", "pool.jsonl")
	default:
		return "", false, errors.New("invalid credential consumer")
	}
	if !pathWithin(destination, runtime.config.ConsumerRoot) {
		return "", false, errors.New("credential consumer escapes its root")
	}
	return destination, true, nil
}

func (runtime *Runtime) detectConsumer(path string) string {
	clean := filepath.Clean(path)
	staging := filepath.Join(runtime.config.ConsumerRoot, "staging") + string(filepath.Separator)
	switch {
	case strings.HasPrefix(clean, staging) && strings.HasSuffix(clean, ".env"):
		return "staging-env"
	case clean == filepath.Join(runtime.config.ConsumerRoot, "qa-pool", "secrets.env"):
		return "qa-secrets"
	case clean == filepath.Join(runtime.config.ConsumerRoot, "qa-pool", "pool.jsonl"):
		return "qa-pool"
	default:
		return "none"
	}
}

func (runtime *Runtime) detectZone(path string) string {
	if runtime.detectConsumer(path) == "staging-env" {
		return strings.TrimSuffix(filepath.Base(path), ".env")
	}
	return "global"
}

func (runtime *Runtime) checkExclusive(ctx context.Context, zone string) error {
	if !runtime.Initialized() {
		return nil
	}
	revisions, err := runtime.records(ctx, sharedLedger)
	if err != nil {
		return err
	}
	identity, err := runtime.Identity()
	if err != nil {
		return err
	}
	for _, credentialID := range credentialIDs(revisions) {
		heads := credential.Heads(revisions, credentialID)
		matches := false
		for _, head := range heads {
			matches = matches || head.Exclusive && head.Zone == zone && head.State == "active"
		}
		if !matches {
			continue
		}
		if len(heads) != 1 {
			return fmt.Errorf("exclusive credential %q has unresolved heads", credentialID)
		}
		head := heads[0]
		trusted, last := false, int64(0)
		if head.AuthorityHost != identity.ActorID {
			if peer, found, err := runtime.peerByActor(head.AuthorityHost); err != nil {
				return err
			} else if found {
				trusted = true
				state, err := runtime.readState(peer.Name)
				if err != nil {
					return err
				}
				last = state.LastSuccess
			}
		}
		decision, err := credential.CheckExclusiveAccess(head, identity.ActorID, runtime.currentYardID(),
			trusted, last, time.Now().Unix(),
			time.Duration(positiveInt(runtime.env["SUBYARD_KEYS_AUTHORITY_MAX_AGE"], 3600))*time.Second)
		if err != nil {
			return fmt.Errorf("exclusive credential %q has invalid access metadata", credentialID)
		}
		switch decision.Reason {
		case "authority-local", "authority-fresh":
		case "not-assigned":
			return fmt.Errorf("exclusive credential %q is assigned to another yard", credentialID)
		case "authority-untrusted":
			return fmt.Errorf("exclusive authority for %q is not trusted", credentialID)
		case "authority-stale":
			return fmt.Errorf("authority grant for %q is stale; sync keys before start", credentialID)
		default:
			return fmt.Errorf("exclusive credential %q has an invalid access decision", credentialID)
		}
	}
	return nil
}

func (runtime *Runtime) currentYardID() string {
	return runtime.actorID() + "/" + runtime.config.Context
}

func (runtime *Runtime) actorID() string {
	identity, _ := runtime.Identity()
	return identity.ActorID
}

func (runtime *Runtime) assignmentExec(ctx context.Context, assignment string, arguments ...string) ([]byte, error) {
	actor, contextName, err := splitAssignment(assignment)
	if err != nil {
		return nil, err
	}
	identity, err := runtime.Identity()
	if err != nil {
		return nil, err
	}
	if actor == identity.ActorID {
		return runtime.callTarget(ctx, Target{Name: contextName, Transport: "local"}, arguments, nil)
	}
	peer, found, err := runtime.peerByActor(actor)
	if err != nil || !found {
		return nil, firstError(err, errors.New("assignment host is not enrolled"))
	}
	return runtime.callPeerContext(ctx, peer, contextName, arguments)
}

func (runtime *Runtime) stopAssignedConsumer(ctx context.Context, assignment, zone string) error {
	output, err := runtime.assignmentExec(ctx, assignment, "staging", "status", zone)
	if err != nil {
		return err
	}
	if strings.Contains(string(output), "gateway: running") {
		_, err = runtime.assignmentExec(ctx, assignment, "staging", "stop", zone, "--yes")
	}
	return err
}

func (runtime *Runtime) syncAssignmentHost(ctx context.Context, assignment string) error {
	actor, _, err := splitAssignment(assignment)
	if err != nil {
		return err
	}
	identity, err := runtime.Identity()
	if err != nil || actor == identity.ActorID {
		return err
	}
	peer, found, err := runtime.peerByActor(actor)
	if err != nil || !found {
		return firstError(err, errors.New("assignment host is not enrolled"))
	}
	return runtime.syncPeer(ctx, peer.Name)
}

func (runtime *Runtime) refreshAssignment(ctx context.Context, assignment string) error {
	_, err := runtime.assignmentExec(ctx, assignment, "_keys-exchange", "refresh", runtime.actorID())
	return err
}

func validateClassification(label, kind, zone, consumerName string) error {
	if label == "" || len(label) > 128 || strings.ContainsAny(label, "\r\n") {
		return errors.New("credential label is invalid")
	}
	if !domain.SafeID(kind) {
		return fmt.Errorf("invalid credential kind %q", kind)
	}
	if !validZone(zone) {
		return fmt.Errorf("invalid credential zone %q", zone)
	}
	if zone == "prod" || zone == "production" {
		return errors.New("production credentials are outside the Subyard credential ledger scope")
	}
	if !contains([]string{"none", "staging-env", "qa-secrets", "qa-pool"}, consumerName) {
		return fmt.Errorf("invalid consumer %q", consumerName)
	}
	return nil
}

func validZone(zone string) bool { return domain.SafeID(zone) && zone != "." && zone != ".." }

func decodeIdentity(payload []byte) (Identity, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var identity Identity
	if err := decoder.Decode(&identity); err != nil {
		return Identity{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Identity{}, errors.New("identity has trailing data")
	}
	return identity, validateIdentity(identity)
}

func signingFingerprint(public string) (string, error) {
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(public))
	if err != nil || len(bytes.TrimSpace(rest)) != 0 {
		return "", errors.New("peer signing key is invalid")
	}
	return ssh.FingerprintSHA256(key), nil
}

func credentialIDs(revisions []domain.CredentialMetadata) []string {
	set := make(map[string]struct{})
	for _, revision := range revisions {
		set[revision.CredentialID] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for credentialID := range set {
		result = append(result, credentialID)
	}
	sort.Strings(result)
	return result
}

func headIDs(heads []domain.CredentialMetadata) []string {
	result := make([]string, 0, len(heads))
	for _, head := range heads {
		result = append(result, head.RevisionID)
	}
	sort.Strings(result)
	return result
}

func (runtime *Runtime) headSnapshot(ctx context.Context) ([]string, error) {
	all, err := runtime.allRecords(ctx)
	if err != nil {
		return nil, err
	}
	var snapshot []string
	for _, scope := range []ledgerScope{sharedLedger, localLedger} {
		for _, credentialID := range credentialIDs(all[scope]) {
			heads := credential.Heads(all[scope], credentialID)
			for _, head := range heads {
				payload, err := json.Marshal(head)
				if err != nil {
					return nil, err
				}
				snapshot = append(snapshot, string(scope)+":"+credentialID+":"+string(payload))
			}
		}
	}
	return snapshot, nil
}

func sameCredentialMetadata(left, right domain.CredentialMetadata) bool {
	leftPayload, leftErr := json.Marshal(left)
	rightPayload, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftPayload, rightPayload)
}

func withoutYes(arguments []string) []string {
	result := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		if argument != "-y" && argument != "--yes" {
			result = append(result, argument)
		}
	}
	return result
}

func onlyYes(arguments []string) error {
	if len(withoutYes(arguments)) != 0 {
		return errors.New("unexpected arguments")
	}
	return nil
}

func optionalValue(arguments []string) (string, error) {
	values := withoutYes(arguments)
	if len(values) > 1 {
		return "", errors.New("too many arguments")
	}
	if len(values) == 1 {
		return values[0], nil
	}
	return "", nil
}

func requiredValue(arguments []string, message string) (string, error) {
	values := withoutYes(arguments)
	if len(values) != 1 {
		return "", errors.New(message)
	}
	return values[0], nil
}

func parseCredentialAndFile(arguments []string) (string, string, error) {
	values := withoutYes(arguments)
	credentialID, source := "", ""
	for index := 0; index < len(values); index++ {
		if values[index] == "--file" {
			index++
			if index >= len(values) {
				return "", "", errors.New("--file needs a path")
			}
			source = values[index]
			continue
		}
		if strings.HasPrefix(values[index], "-") || credentialID != "" {
			return "", "", fmt.Errorf("unexpected argument %q", values[index])
		}
		credentialID = values[index]
	}
	if !validCredentialID(credentialID) {
		return "", "", errors.New("a valid credential ID is required")
	}
	return credentialID, source, nil
}

func splitAssignment(assignment string) (string, string, error) {
	actor, contextName, found := strings.Cut(assignment, "/")
	if !found || !domain.SafeID(actor) || !domain.SafeName(contextName) {
		return "", "", errors.New("invalid credential assignment")
	}
	return actor, contextName, nil
}

func contains[T comparable](values []T, wanted T) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func firstError(primary, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}

func ageHuman(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	switch {
	case seconds < 60:
		return strconv.FormatInt(seconds, 10) + "s"
	case seconds < 3600:
		return strconv.FormatInt(seconds/60, 10) + "m"
	case seconds < 86400:
		return strconv.FormatInt(seconds/3600, 10) + "h"
	default:
		return strconv.FormatInt(seconds/86400, 10) + "d"
	}
}
