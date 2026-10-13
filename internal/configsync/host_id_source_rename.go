package configsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
)

// A source rename never changes setting content or an operator's local overrides.
// The prepared commit is initially unreachable. HEAD is the durable commit point.
type hostSourceRename struct {
	IndexEntries []byte
	AfterIndex   []byte
	GitDir       string
	HeadRef      string
	Root         string
	SourceHostID string
	BeforeCommit string
	AfterCommit  string
	BeforeTree   string
	AfterTree    string
	RecordDigest string
	Paths        []string
	Local        []localInputObservation
	Cache        []renameCacheFile
}
type renameCacheFile struct {
	Path    string
	Before  localInputObservation
	Content []byte
	Mode    uint32
}

func PrepareHostIDRenameWithSource(options Options, newHostID, sourceHostID string) (HostIDRenamePlan, error) {
	plan, err := prepareHostIDRename(options.ConfigHome, newHostID, sourceHostID != "")
	if err != nil {
		return plan, err
	}
	plan.options = options
	plan.sourceHostID = sourceHostID
	record, exists, err := readSourceRecord(options.ConfigHome)
	if err != nil {
		return plan, err
	}
	if !exists {
		if sourceHostID != "" {
			return plan, errors.New("source HostID recovery requires a registered configuration source")
		}
		return plan, nil
	}
	if options.RepositoryRoot == "" || options.OperatorHome == "" {
		return plan, errors.New("registered source rename requires configuration validation options")
	}
	if sourceHostID == "" {
		sourceHostID = plan.OldHostID
	}
	if !safeHostID(sourceHostID) || sourceHostID == newHostID {
		return plan, errors.New("source HostID must name the preserved old subtree")
	}
	if record.Origin != "" {
		origin, err := gitOutput(record.Checkout, "remote", "get-url", "origin")
		if err != nil || strings.TrimSpace(origin) != record.Origin {
			return plan, errors.New("source origin differs from its registration")
		}
	}
	options.SourceRoot = record.Checkout
	if err := validateSourceDirectory(record.Checkout); err != nil {
		return plan, err
	}
	if _, err := validateGitSource(record.Checkout, sourceHostID); err != nil {
		return plan, err
	}
	gitDir, err := gitOutput(record.Checkout, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return plan, err
	}
	gitDir = strings.TrimSpace(gitDir)
	info, err := os.Lstat(gitDir)
	if err != nil {
		return plan, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return plan, errors.New("unsafe source Git metadata directory")
	}
	if err := validateOwnedMode(gitDir, info, true); err != nil {
		return plan, err
	}
	if _, err := os.Lstat(filepath.Join(gitDir, "index.lock")); err == nil {
		return plan, errors.New("configuration source Git index is locked by another operation")
	} else if !errors.Is(err, os.ErrNotExist) {
		return plan, err
	}
	headRef, _ := gitOutput(record.Checkout, "symbolic-ref", "-q", "HEAD")
	headRef = strings.TrimSpace(headRef)
	status, err := gitOutput(record.Checkout, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return plan, err
	}
	if status != "" {
		return plan, errors.New("configuration source must be clean before owner rename")
	}
	if ignored, err := gitOutput(record.Checkout, "ls-files", "--others", "--ignored", "--exclude-standard"); err != nil || ignored != "" {
		return plan, errors.New("configuration source contains ignored files; rename refused")
	}
	oldPath, newPath := filepath.Join(record.Checkout, "hosts", sourceHostID), filepath.Join(record.Checkout, "hosts", newHostID)
	if plan.OldHostID == newHostID && sourceHostID != "" {
		already, err := completedSourceRename(options, record, sourceHostID, newHostID)
		if err != nil {
			return plan, err
		}
		if already {
			plan.AlreadyMigrated = true
			return plan, nil
		}
	}
	if _, err := os.Lstat(newPath); !errors.Is(err, os.ErrNotExist) {
		return plan, errors.New("destination host subtree already exists or cannot be inspected")
	}
	if _, err := readOptionalDirectory(record.Checkout, oldPath); err != nil {
		return plan, err
	}
	if _, err := os.Lstat(oldPath); errors.Is(err, os.ErrNotExist) {
		if sourceHostID != plan.OldHostID {
			return plan, errors.New("selected source HostID has no preserved subtree")
		}
		previous, err := readManifest(options.ConfigHome)
		if err != nil {
			return plan, err
		}
		if err := guardHostSourceIdentity(record.Checkout, plan.OldHostID, previous); err != nil {
			return plan, err
		}
		return plan, nil
	} else if err != nil {
		return plan, err
	}
	catalog, err := config.LoadCatalog(options.RepositoryRoot)
	if err != nil {
		return plan, err
	}
	source, err := readSource(options, sourceHostID, catalog)
	if err != nil {
		return plan, err
	}
	previous, err := readManifest(options.ConfigHome)
	if err != nil {
		return plan, err
	}
	if previous.SchemaVersion == 1 {
		return plan, errors.New("sync legacy configuration before renaming its owner")
	}
	if previous.SchemaVersion != 0 && previous.SourceID != source.id {
		return plan, errors.New("registered source does not match the applied source")
	}
	if sourceHostID == plan.OldHostID && previous.SchemaVersion != 0 && previous.SourceCommit != source.commit {
		return plan, errors.New("sync the current source commit before renaming its owner")
	}
	if err := prepareRenameSSHCache(options, &source, previous); err != nil {
		return plan, fmt.Errorf("rename configuration: %w", err)
	}
	recordBytes, err := os.ReadFile(SourceRecordPath(options.ConfigHome))
	if err != nil {
		return plan, err
	}
	transaction := &hostSourceRename{Root: record.Checkout, SourceHostID: sourceHostID, BeforeCommit: source.commit, RecordDigest: digestBytes(recordBytes), GitDir: gitDir, HeadRef: headRef}
	transaction.Local, err = observeLocalInputs(options.ConfigHome, source, previous)
	if err != nil {
		return plan, err
	}
	for path, file := range source.files {
		before, err := observeLocalInput(options.ConfigHome, path)
		if err != nil {
			return plan, err
		}
		if before.Exists && (before.Digest != file.Digest || before.Mode != uint32(file.Mode)) {
			return plan, fmt.Errorf("managed cache %s differs from preserved source", path)
		}
		transaction.Cache = append(transaction.Cache, renameCacheFile{Path: path, Before: before, Content: file.Content, Mode: uint32(file.Mode)})
	}
	sort.Slice(transaction.Cache, func(i, j int) bool { return transaction.Cache[i].Path < transaction.Cache[j].Path })
	listing, err := gitOutputBytes(record.Checkout, "ls-tree", "-r", "-z", "HEAD")
	if err != nil {
		return plan, err
	}
	var transformed bytes.Buffer
	oldPrefix, newPrefix := "hosts/"+sourceHostID+"/", "hosts/"+newHostID+"/"
	for _, entry := range bytes.Split(listing, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		parts := bytes.SplitN(entry, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return plan, errors.New("invalid source tree")
		}
		path := string(parts[1])
		if strings.HasPrefix(path, oldPrefix) {
			transaction.Paths = append(transaction.Paths, path+" -> "+newPrefix+strings.TrimPrefix(path, oldPrefix))
			path = newPrefix + strings.TrimPrefix(path, oldPrefix)
		}
		// update-index accepts mode and object ID, without ls-tree's object type.
		fields := strings.Fields(string(parts[0]))
		if len(fields) != 3 {
			return plan, errors.New("invalid source tree entry")
		}
		fmt.Fprintf(&transformed, "%s %s\t%s%c", fields[0], fields[2], path, 0)
	}
	if len(transaction.Paths) == 0 {
		return plan, errors.New("source host subtree has no tracked settings")
	}
	for _, kind := range []string{"GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"} {
		if _, err := gitOutput(record.Checkout, "var", kind); err != nil {
			return plan, errors.New("configure Git author and committer identity before renaming the owner")
		}
	}
	transaction.IndexEntries = transformed.Bytes()
	transaction.BeforeTree, err = gitOutput(record.Checkout, "rev-parse", "HEAD^{tree}")
	transaction.BeforeTree = strings.TrimSpace(transaction.BeforeTree)
	if err != nil {
		return plan, err
	}
	manifest := Manifest{SchemaVersion: manifestSchema, Generation: previous.Generation + 1, SourceID: source.id, SourceCommit: transaction.BeforeCommit, HostID: newHostID, SourceSchema: sourceSchema, SourceDigest: source.digest}
	for _, file := range transaction.Cache {
		manifest.Files = append(manifest.Files, ManagedFile{Path: file.Path, Digest: digestBytes(file.Content), Mode: file.Mode, Generation: manifest.Generation})
	}
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return plan, err
	}
	plan.ManifestChanged = true
	plan.manifestAfter = append(payload, '\n')
	plan.source = transaction
	plan.options = options
	plan.sourceHostID = sourceHostID
	return plan, nil
}

// No automatic foreign-subtree inference: an empty selected scope is suspicious
// only when preserved foreign settings were already present at the applied commit.
// A committed deletion of this owner's subtree remains an intentional removal.
func guardHostSourceIdentity(root, hostID string, previous Manifest) error {
	if _, err := os.Lstat(filepath.Join(root, "hosts", hostID)); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := readOptionalDirectory(root, filepath.Join(root, "hosts"))
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	if previous.SchemaVersion == 0 {
		return nil
	}
	baseline, err := selectedCommitDigest(root, previous.SourceCommit, hostID)
	if err != nil {
		return err
	}
	if baseline == previous.SourceDigest {
		return nil
	}
	return errors.New("selected owner subtree is absent while preserved owner settings exist; use yard host rename <current-host-id> --source-host-id <old-host-id> to recover explicitly")
}

func sameSourceRenameBaseline(a, b *hostSourceRename) bool {
	if a == nil || b == nil {
		return a == b
	}
	left, right := *a, *b
	left.AfterCommit = ""
	right.AfterCommit = ""
	x, _ := json.Marshal(left)
	y, _ := json.Marshal(right)
	return bytes.Equal(x, y)
}

func recoverHostSourceRename(configHome string, journal hostIDRenameJournal) error {
	return recoverHostSourceRenameAtCheckpoint(configHome, journal, nil)
}

func recoverHostSourceRenameAtCheckpoint(configHome string, journal hostIDRenameJournal, checkpoint func(string) error) error {
	source := journal.Source
	if source == nil || !safeHostID(source.SourceHostID) || source.SourceHostID == journal.NewHostID || !validGitOID(source.BeforeCommit) || !validGitOID(source.AfterCommit) || !validGitOID(source.BeforeTree) || !validGitOID(source.AfterTree) {
		return errors.New("invalid source rename journal")
	}
	record, exists, err := readSourceRecord(configHome)
	if err != nil {
		return err
	}
	if !exists || record.Checkout != source.Root {
		return errors.New("source registration changed during rename")
	}
	recordBytes, err := os.ReadFile(SourceRecordPath(configHome))
	if err != nil {
		return err
	}
	if digestBytes(recordBytes) != source.RecordDigest {
		return errors.New("source registration changed during rename")
	}
	if err := validateSourceDirectory(source.Root); err != nil {
		return err
	}
	identity, err := os.ReadFile(HostIDPath(configHome))
	if err != nil {
		return err
	}
	if digestBytes(identity) != journal.HostIDDigest && string(identity) != journal.NewHostID+"\n" {
		return errors.New("owner identity changed during rename")
	}
	manifest, err := os.ReadFile(ManifestPath(configHome))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !bytes.Equal(manifest, journal.ManifestBefore) && !bytes.Equal(manifest, journal.ManifestAfter) {
		return errors.New("manifest changed during rename")
	}
	head, err := gitOutput(source.Root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	head = strings.TrimSpace(head)
	if head != source.BeforeCommit && head != source.AfterCommit {
		return errors.New("source HEAD changed during rename; retain journal and reconcile the source before recovery")
	}
	for _, before := range source.Local {
		now, err := observeLocalInput(configHome, before.Path)
		if err != nil {
			return err
		}
		if now != before && head != source.AfterCommit {
			if err := rollbackUnpublishedSourceRename(configHome, journal); err != nil {
				return fmt.Errorf("local override changed; source rollback refused: %w", err)
			}
			return fmt.Errorf("%w: local override %s changed; unpublished rename restored the old owner/source pair", ErrPlanStale, before.Path)
		}
	}
	for _, file := range source.Cache {
		if !safeRelative(file.Path) || !strings.HasPrefix(file.Path, config.GitSettingsRelativePath+"/") {
			return errors.New("invalid rename cache path")
		}
		if _, valid := managedSettingRole(localSettingsPath(file.Path)); !valid {
			return errors.New("invalid rename cache role")
		}
		now, err := observeLocalInput(configHome, file.Path)
		if err != nil {
			return err
		}
		if now != file.Before && !(now.Exists && now.Digest == digestBytes(file.Content) && now.Mode == file.Mode) {
			return fmt.Errorf("managed cache %s changed during rename", file.Path)
		}
	}
	gitDir, err := gitOutput(source.Root, "rev-parse", "--absolute-git-dir")
	if err != nil || strings.TrimSpace(gitDir) != source.GitDir {
		return errors.New("source Git metadata directory changed during rename")
	}
	headRef, _ := gitOutput(source.Root, "symbolic-ref", "-q", "HEAD")
	if strings.TrimSpace(headRef) != source.HeadRef {
		return errors.New("source HEAD reference changed during rename")
	}
	releaseIndex, err := lockRenameIndex(source)
	if err != nil {
		return err
	}
	defer releaseIndex()
	indexImage, err := os.Lstat(renameIndexImage(source))
	if err != nil {
		return err
	}
	index, err := inspectRenameIndexTree(source)
	if err != nil {
		return err
	}
	index = strings.TrimSpace(index)
	if index != source.BeforeTree && index != source.AfterTree {
		return errors.New("source index changed during rename")
	}
	oldPath, newPath := filepath.Join(source.Root, "hosts", source.SourceHostID), filepath.Join(source.Root, "hosts", journal.NewHostID)
	_, oldErr := os.Lstat(oldPath)
	_, newErr := os.Lstat(newPath)
	if oldErr == nil && errors.Is(newErr, os.ErrNotExist) {
		if err := verifyRenameSubtree(source.Root, source.BeforeCommit, source.SourceHostID, source.SourceHostID); err != nil {
			return err
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Dir(oldPath)); err != nil {
			return err
		}
	} else if errors.Is(oldErr, os.ErrNotExist) && newErr == nil {
		if err := verifyRenameSubtree(source.Root, source.BeforeCommit, source.SourceHostID, journal.NewHostID); err != nil {
			return err
		}
	} else {
		return errors.New("source rename paths collided or disappeared; retain journal for recovery")
	}
	if head == source.BeforeCommit {
		if _, err := gitOutput(source.Root, "update-ref", "HEAD", source.AfterCommit, source.BeforeCommit); err != nil {
			return err
		}
	}
	// Only these two known index trees are admitted, never an unrelated staged edit.
	if index == source.BeforeTree {
		if checkpoint != nil {
			if err := checkpoint("index-publication"); err != nil {
				return err
			}
		}
		if err := publishRenameIndex(source, indexImage, index); err != nil {
			return err
		}
	}
	for _, file := range source.Cache {
		if err := writeFileDurable(configHome, filepath.Join(configHome, file.Path), file.Content, os.FileMode(file.Mode)); err != nil {
			return err
		}
	}
	if err := writeFileDurable(configHome, ManifestPath(configHome), journal.ManifestAfter, 0o600); err != nil {
		return err
	}
	if err := writeFileDurable(configHome, HostIDPath(configHome), []byte(journal.NewHostID+"\n"), 0o600); err != nil {
		return err
	}
	receipt, _ := json.Marshal(completedHostRename{SourceHostID: source.SourceHostID, NewHostID: journal.NewHostID, Root: source.Root, RecordDigest: source.RecordDigest, Commit: source.AfterCommit})
	if err := writeFileDurable(configHome, filepath.Join(configHome, ".sync", "host-id-rename-complete.json"), append(receipt, '\n'), 0o600); err != nil {
		return err
	}
	releaseIndex()
	if err := cleanupHostIDRename(configHome); err != nil {
		return err
	}
	return cleanupRenameIndex(source)
}

func (plan HostIDRenamePlan) SourceConsequences() []string {
	if plan.source == nil {
		return nil
	}
	result := []string{"create a separate local Git commit preserving all setting content and modes; no push"}
	for _, path := range plan.source.Paths {
		result = append(result, "move "+path)
	}
	return result
}

func selectedCommitDigest(root, commit, hostID string) (string, error) {
	listing, err := gitOutputBytes(root, "ls-tree", "-r", "-z", commit, "--", "shared", "hosts/"+hostID)
	if err != nil {
		return "", err
	}
	files := map[string]candidateFile{}
	for _, entry := range bytes.Split(listing, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		parts := bytes.SplitN(entry, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return "", errors.New("invalid recorded source tree")
		}
		path := string(parts[1])
		target := ""
		switch {
		case path == "shared/config.env":
			target = "overrides/shared/config.env"
		case strings.HasPrefix(path, "shared/overrides/agents/"):
			target = "overrides/shared/agents/" + strings.TrimPrefix(path, "shared/overrides/agents/")
		case path == "hosts/"+hostID+"/config.env":
			target = "config.env"
		case strings.HasPrefix(path, "hosts/"+hostID+"/overrides/agents/"):
			target = "overrides/host/agents/" + strings.TrimPrefix(path, "hosts/"+hostID+"/overrides/agents/")
		case strings.HasPrefix(path, "hosts/"+hostID+"/yards/"):
			target = "yards/" + strings.TrimPrefix(path, "hosts/"+hostID+"/yards/")
		}
		if target == "" {
			continue
		}
		content, err := gitOutputBytes(root, "show", commit+":"+path)
		if err != nil {
			return "", err
		}
		files[gitSettingsPath(target)] = candidateFile{Digest: digestBytes(content)}
	}
	manifest, err := gitOutputBytes(root, "show", commit+":subyard-config.json")
	if err != nil {
		return "", err
	}
	return snapshotDigest(files, manifest), nil
}

func verifyRenameSubtree(root, commit, oldID, currentID string) error {
	listing, err := gitOutputBytes(root, "ls-tree", "-r", "-z", commit, "--", "hosts/"+oldID)
	if err != nil {
		return err
	}
	expected := map[string]string{}
	for _, entry := range bytes.Split(listing, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		parts := bytes.SplitN(entry, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return errors.New("invalid rename source tree")
		}
		fields := strings.Fields(string(parts[0]))
		if len(fields) != 3 || fields[1] != "blob" || fields[0] != "100644" {
			return errors.New("unsafe rename source tree mode")
		}
		relative := strings.TrimPrefix(string(parts[1]), "hosts/"+oldID+"/")
		expected[relative] = fields[2]
	}
	subtree := filepath.Join(root, "hosts", currentID)
	err = filepath.WalkDir(subtree, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return validateOwnedMode(path, info, true)
		}
		relative, err := filepath.Rel(subtree, path)
		if err != nil {
			return err
		}
		oid, ok := expected[filepath.ToSlash(relative)]
		if !ok {
			return errors.New("rename subtree contains unrelated files")
		}
		if _, err := readSourceFile(root, path, maxAssetBytes, false); err != nil {
			return err
		}
		actual, err := gitOutput(root, "hash-object", "--", path)
		if err != nil {
			return err
		}
		if strings.TrimSpace(actual) != oid {
			return errors.New("rename source setting content changed")
		}
		delete(expected, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return err
	}
	if len(expected) != 0 {
		return errors.New("rename source setting disappeared")
	}
	return nil
}

func finalizeSourceRename(plan *HostIDRenamePlan) error {
	transaction := plan.source
	// Isolated index: planning never stages or rewrites the operator's index.
	index, err := os.CreateTemp("/tmp", "subyard-rename-index-")
	if err != nil {
		return err
	}
	indexPath := index.Name()
	index.Close()
	os.Remove(indexPath)
	defer os.Remove(indexPath)
	run := func(input []byte, args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", transaction.Root, "-c", "commit.gpgSign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+indexPath, "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
		cmd.Stdin = bytes.NewReader(input)
		out, err := cmd.Output()
		if err != nil {
			return "", errors.New("prepare source rename Git object failed")
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err = run(nil, "read-tree", "--empty"); err != nil {
		return err
	}
	if _, err = run(transaction.IndexEntries, "update-index", "-z", "--index-info"); err != nil {
		return err
	}
	transaction.AfterTree, err = run(nil, "write-tree")
	if err != nil {
		return err
	}
	transaction.BeforeTree, err = gitOutput(transaction.Root, "rev-parse", "HEAD^{tree}")
	transaction.BeforeTree = strings.TrimSpace(transaction.BeforeTree)
	if err != nil {
		return err
	}
	transaction.AfterCommit, err = run([]byte(fmt.Sprintf("Rename owner settings %s to %s\n", transaction.SourceHostID, plan.NewHostID)), "commit-tree", transaction.AfterTree, "-p", transaction.BeforeCommit)
	if err != nil {
		return err
	}

	transaction.AfterIndex, err = os.ReadFile(indexPath)
	if err != nil {
		return err
	}
	var manifest Manifest
	if err := json.Unmarshal(plan.manifestAfter, &manifest); err != nil {
		return err
	}
	manifest.SourceCommit = transaction.AfterCommit
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	plan.manifestAfter = append(payload, '\n')
	return nil
}

func renameIndexImage(source *hostSourceRename) string {
	return filepath.Join(source.GitDir, "subyard-host-rename-"+source.AfterCommit+".index")
}

// A hard link proves ownership of index.lock across process death. An unrelated
// Git lock can neither be adopted nor removed, even if its content is identical.
func lockRenameIndex(source *hostSourceRename) (func(), error) {
	if len(source.AfterIndex) == 0 || !filepath.IsAbs(source.GitDir) {
		return nil, errors.New("source rename index journal is invalid")
	}
	imagePath := renameIndexImage(source)
	if _, err := os.Lstat(imagePath); errors.Is(err, os.ErrNotExist) {
		file, err := os.CreateTemp(source.GitDir, "subyard-rename-index-")
		if err != nil {
			return nil, err
		}
		temporary := file.Name()
		defer os.Remove(temporary)
		if err = file.Chmod(0o600); err == nil {
			_, err = file.Write(source.AfterIndex)
		}
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
		if err := os.Link(temporary, imagePath); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if err := syncDirectory(source.GitDir); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	image, err := os.Lstat(imagePath)
	if err != nil {
		return nil, err
	}
	if !image.Mode().IsRegular() || image.Mode().Perm() != 0o600 {
		return nil, errors.New("source rename index image is unsafe")
	}
	if err := validateOwnedMode(imagePath, image, false); err != nil {
		return nil, err
	}
	content, err := os.ReadFile(imagePath)
	if err != nil || !bytes.Equal(content, source.AfterIndex) {
		return nil, errors.New("source rename index image changed")
	}
	lockPath := filepath.Join(source.GitDir, "index.lock")
	if err := os.Link(imagePath, lockPath); err != nil {
		lock, inspectErr := os.Lstat(lockPath)
		if inspectErr != nil || !os.SameFile(image, lock) {
			return nil, errors.New("source Git index is locked by another operation; retain rename journal and retry after it finishes")
		}
	}
	release := func() {
		current, err := os.Lstat(lockPath)
		if err == nil && os.SameFile(image, current) {
			_ = os.Remove(lockPath)
			_ = syncDirectory(source.GitDir)
		}
	}
	return release, nil
}
func cleanupRenameIndex(source *hostSourceRename) error {
	path := renameIndexImage(source)
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(content, source.AfterIndex) {
		return errors.New("source rename index image changed before cleanup")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(source.GitDir)
}

func inspectRenameIndexTree(source *hostSourceRename) (string, error) {
	path := filepath.Join(source.GitDir, "index")
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("unsafe source Git index")
	}
	if err := validateOwnedMode(path, info, false); err != nil {
		return "", err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp("/tmp", "subyard-rename-index-inspect-")
	if err != nil {
		return "", err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	_, err = file.Write(content)
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	command := exec.Command("git", "-C", source.Root, "write-tree")
	command.Env = append(os.Environ(), "GIT_INDEX_FILE="+temporary, "GIT_OPTIONAL_LOCKS=0")
	output, err := command.Output()
	if err != nil {
		return "", errors.New("cannot inspect source Git index tree")
	}
	return strings.TrimSpace(string(output)), nil
}

type completedHostRename struct{ SourceHostID, NewHostID, Root, RecordDigest, Commit string }

func completedSourceRename(options Options, record SourceRecord, sourceHostID, newHostID string) (bool, error) {
	path := filepath.Join(options.ConfigHome, ".sync", "host-id-rename-complete.json")
	snapshot, err := config.ReadPersistentFileSnapshot(options.ConfigHome, path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !snapshot.Exists {
		return false, nil
	}
	var receipt completedHostRename
	decoder := json.NewDecoder(bytes.NewReader(snapshot.Content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return false, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return false, err
	}
	recordBytes, err := os.ReadFile(SourceRecordPath(options.ConfigHome))
	if err != nil {
		return false, err
	}
	if receipt.SourceHostID != sourceHostID || receipt.NewHostID != newHostID || receipt.Root != record.Checkout || receipt.RecordDigest != digestBytes(recordBytes) || !validGitOID(receipt.Commit) {
		return false, nil
	}
	if _, err := os.Lstat(filepath.Join(record.Checkout, "hosts", sourceHostID)); !errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if _, err := gitOutput(record.Checkout, "merge-base", "--is-ancestor", receipt.Commit, "HEAD"); err != nil {
		return false, nil
	}
	catalog, err := config.LoadCatalog(options.RepositoryRoot)
	if err != nil {
		return false, err
	}
	source, err := readSource(options, newHostID, catalog)
	if err != nil {
		return false, err
	}
	manifest, err := readManifest(options.ConfigHome)
	if err != nil {
		return false, err
	}
	return manifest.HostID == newHostID && manifest.SourceID == source.id && manifest.SourceCommit == source.commit && manifest.SourceDigest == source.digest, nil
}

func rollbackUnpublishedSourceRename(configHome string, journal hostIDRenameJournal) error {
	source := journal.Source
	index, err := inspectRenameIndexTree(source)
	if err != nil || index != source.BeforeTree {
		return errors.New("source index changed before rollback")
	}
	oldPath, newPath := filepath.Join(source.Root, "hosts", source.SourceHostID), filepath.Join(source.Root, "hosts", journal.NewHostID)
	_, oldErr := os.Lstat(oldPath)
	_, newErr := os.Lstat(newPath)
	if errors.Is(oldErr, os.ErrNotExist) && newErr == nil {
		if err := verifyRenameSubtree(source.Root, source.BeforeCommit, source.SourceHostID, journal.NewHostID); err != nil {
			return err
		}
		if err := os.Rename(newPath, oldPath); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Dir(oldPath)); err != nil {
			return err
		}
	} else if oldErr != nil || !errors.Is(newErr, os.ErrNotExist) {
		return errors.New("source paths changed before rollback")
	}
	head, err := gitOutput(source.Root, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) != source.BeforeCommit {
		return errors.New("source HEAD changed before rollback")
	}
	image, err := os.Lstat(renameIndexImage(source))
	if err == nil {
		lockPath := filepath.Join(source.GitDir, "index.lock")
		lock, err := os.Lstat(lockPath)
		if err == nil && os.SameFile(image, lock) {
			if err := os.Remove(lockPath); err != nil {
				return err
			}
		}
		if err := cleanupRenameIndex(source); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return cleanupHostIDRename(configHome)
}

// Publish a separate captured image, retaining index.lock even if an external
// actor replaces it. Check inode ownership and index baseline immediately before
// publication; a foreign lock is never consumed as the desired Git index.
func publishRenameIndex(source *hostSourceRename, image os.FileInfo, expectedTree string) error {
	check := func() error {
		currentImage, err := os.Lstat(renameIndexImage(source))
		if err != nil || !os.SameFile(image, currentImage) {
			return errors.New("source rename index image was replaced")
		}
		lock, err := os.Lstat(filepath.Join(source.GitDir, "index.lock"))
		if err != nil || !os.SameFile(image, lock) {
			return errors.New("source Git index lock changed before publication; retain journal for recovery")
		}
		tree, err := inspectRenameIndexTree(source)
		if err != nil {
			return err
		}
		if tree != expectedTree {
			return errors.New("source index changed before publication")
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	file, err := os.CreateTemp(source.GitDir, "subyard-rename-index-publish-")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(source.AfterIndex)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	if err := os.Rename(temporary, filepath.Join(source.GitDir, "index")); err != nil {
		return err
	}
	return syncDirectory(source.GitDir)
}
