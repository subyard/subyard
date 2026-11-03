package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

type hardCleanupRetirement struct {
	Schema                                                          int
	Yard, Project, Instance, Bridge, Pool, Volume                   string
	OriginalUUID, NewUUID, Token                                    string
	SourceBinding, StateBinding, PreservationBinding, NativeBinding string
	Bindings                                                        []releasetransition.RegisteredYardJournalBinding
}

func hardRetirementPath(yard domain.Context) string {
	return filepath.Join(filepath.Dir(hardCleanupPath(yard)), "retired.json")
}

func readHardRetirement(yard domain.Context) (*hardCleanupRetirement, error) {
	path := hardRetirementPath(yard)
	if err := cleanupAncestors(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 || info.Size() > 32<<20 {
		return nil, errors.New("unsafe hard cleanup retirement receipt")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 32<<20))
	decoder.DisallowUnknownFields()
	var receipt hardCleanupRetirement
	if err := decoder.Decode(&receipt); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF || receipt.Schema != 1 || receipt.Yard != yard.YardName || receipt.Project != yard.IncusProject || receipt.Instance != yard.YardInstanceName || receipt.Bridge != yard.IncusBridge || !domain.SafeID(receipt.Token) || len(receipt.Token) != 32 || receipt.OriginalUUID == "" || receipt.NewUUID == "" || receipt.NewUUID == receipt.OriginalUUID || receipt.SourceBinding == "" || receipt.StateBinding == "" || receipt.PreservationBinding == "" || receipt.NativeBinding == "" || len(receipt.Bindings) > 32 {
		return nil, errors.New("invalid hard cleanup retirement receipt")
	}
	return &receipt, nil
}

func (cli *CLI) hardRetiredBinding(ctx context.Context, loaded config.Loaded, journal releasetransition.JournalRecord) (bool, error) {
	receipt, err := readHardRetirement(loaded.Context)
	if err != nil || receipt == nil {
		return false, err
	}
	matched := false
	for _, binding := range receipt.Bindings {
		matched = matched || binding.Matches(journal, "yard."+loaded.Context.YardName)
	}
	if !matched {
		return false, nil
	}
	native, _ := cli.statusPorts()
	instance, err := native.Instance(ctx, receipt.Project, receipt.Instance)
	if err != nil {
		return false, err
	}
	for key, want := range map[string]string{"volatile.uuid": receipt.NewUUID, "user.subyard.cleanup": receipt.Token, "user.subyard.managed": "true", "user.subyard.name": receipt.Yard, "user.subyard.bridge": receipt.Bridge} {
		if instance.Config[key] != want || instance.LocalConfig[key] != want {
			return false, nil
		}
	}
	srv := instance.Devices["srv"]
	return srv["type"] == "disk" && srv["pool"] == receipt.Pool && srv["source"] == receipt.Volume, nil
}

func (cli *CLI) captureHardRetirement(loaded config.Loaded, journal *hardCleanupJournal, store *releasetransition.POSIXV2Store) error {
	binding, err := store.CaptureRegisteredYardBinding("yard." + loaded.Context.YardName)
	if err != nil {
		return err
	}
	if journal.Retirement != nil {
		if operationStateDigest(*journal.Retirement) != operationStateDigest(binding) {
			return domain.ErrPlanStale
		}
		return nil
	}
	previous, err := readHardRetirement(loaded.Context)
	if err != nil {
		return err
	}
	if previous != nil {
		if previous.NewUUID != journal.Native.OriginalUUID {
			return domain.ErrPlanStale
		}
		journal.RetiredBindings = append(journal.RetiredBindings, previous.Bindings...)
	}
	if binding.Snapshot.Exists {
		found := false
		for _, old := range journal.RetiredBindings {
			found = found || operationStateDigest(old) == operationStateDigest(binding)
		}
		if !found {
			journal.RetiredBindings = append(journal.RetiredBindings, binding)
		}
	}
	if len(journal.RetiredBindings) > 32 {
		return errors.New("hard cleanup retirement chain requires release migration completion before another reset")
	}
	journal.Retirement = &binding
	return nil
}

func (cli *CLI) publishHardRetirement(loaded config.Loaded, journal *hardCleanupJournal) error {
	if journal.Retirement == nil || journal.ReplacementUUID == "" {
		return errors.New("hard cleanup has no bound retirement proof")
	}
	store, err := releasetransition.NewPOSIXV2Store(loaded.Context.Paths.ConfigHome)
	if err != nil {
		return err
	}
	release, err := store.Lock()
	if err != nil {
		return err
	}
	defer release()
	if err := cli.captureHardRetirement(loaded, journal, store); err != nil {
		return err
	}
	receipt := hardCleanupRetirement{Schema: 1, Yard: loaded.Context.YardName, Project: journal.Native.Project, Instance: journal.Native.Instance, Bridge: loaded.Context.IncusBridge, Pool: journal.Native.Pool, Volume: journal.Native.Volume, OriginalUUID: journal.Native.OriginalUUID, NewUUID: journal.ReplacementUUID, Token: journal.Token, Bindings: journal.RetiredBindings, SourceBinding: journal.SourceBinding, StateBinding: journal.StateBinding, PreservationBinding: operationStateDigest(journal.Preserved), NativeBinding: operationStateDigest(journal.Native)}
	return saveCleanupRecord(hardRetirementPath(loaded.Context), receipt)
}
