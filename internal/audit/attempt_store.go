package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/safefs"
)

// attemptStore keeps one private JSON file per attempt in logs/<directory>
// under the Subyard data home and retains the newest attempts by start time.
// The update and setup histories share it.
type attemptStore[T any] struct {
	home        string
	beforeWrite func(context.Context) (func(), bool, error)
	label       string
	directory   string
	lock        string
	temp        string
	retention   int
	maxBytes    int64
	// identity returns the attempt ID that names the file and the start time.
	identity func(T) (string, time.Time)
	valid    func(T) bool
}

func (store attemptStore[T]) write(record T) error {
	if store.beforeWrite != nil {
		release, write, err := store.beforeWrite(context.Background())
		if err != nil {
			return err
		}
		if !write {
			return nil
		}
		defer release()
	}
	logs := filepath.Join(store.home, "logs")
	if err := store.ensurePrivateDirectory(logs); err != nil {
		return err
	}
	directory := filepath.Join(logs, store.directory)
	if err := store.ensurePrivateDirectory(directory); err != nil {
		return err
	}
	lock, err := safefs.Lock(context.Background(), filepath.Join(logs, store.lock), false, 0)
	if err != nil {
		return err
	}
	defer lock.Release()
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if int64(len(payload)) > store.maxBytes {
		return errors.New(store.label + " record is too large")
	}
	id, _ := store.identity(record)
	path := filepath.Join(directory, id+".json")
	if err := safefs.WriteFile(path, payload, safefs.Options{TempPattern: store.temp}); err != nil {
		return err
	}
	return store.prune(directory)
}

// read returns the newest records first, at most limit of them.
func (store attemptStore[T]) read(limit int) ([]T, error) {
	if store.home == "" {
		return nil, errors.New(store.label + " home is required")
	}
	if limit < 1 {
		return nil, errors.New(store.label + " limit must be positive")
	}
	directory := filepath.Join(store.home, "logs", store.directory)
	// Only a missing directory is an empty history; a record that vanished
	// during the read stays an error.
	if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
		return []T{}, nil
	}
	records, _, err := store.list(directory)
	if err != nil {
		return nil, err
	}
	if limit > len(records) {
		limit = len(records)
	}
	return records[:limit], nil
}

// list returns the records of directory and their file names, newest first.
func (store attemptStore[T]) list(directory string) ([]T, []string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, nil, err
	}
	type item struct {
		record  T
		name    string
		id      string
		started time.Time
	}
	items := make([]item, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record, err := store.load(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, nil, err
		}
		id, started := store.identity(record)
		if entry.Name() != id+".json" {
			return nil, nil, errors.New(store.label + " filename does not match attempt ID")
		}
		items = append(items, item{record: record, name: entry.Name(), id: id, started: started})
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].started.Equal(items[right].started) {
			return items[left].id > items[right].id
		}
		return items[left].started.After(items[right].started)
	})
	records := make([]T, len(items))
	names := make([]string, len(items))
	for index, item := range items {
		records[index], names[index] = item.record, item.name
	}
	return records, names, nil
}

func (store attemptStore[T]) prune(directory string) error {
	_, names, err := store.list(directory)
	if err != nil {
		return err
	}
	if len(names) <= store.retention {
		return nil
	}
	for _, name := range names[store.retention:] {
		if err := os.Remove(filepath.Join(directory, name)); err != nil {
			return err
		}
	}
	return nil
}

func (store attemptStore[T]) load(path string) (T, error) {
	var record T
	invalid := errors.New(store.label + " record is not a bounded regular file")
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return record, err
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Size() > store.maxBytes {
		return record, invalid
	}
	file, err := os.Open(path)
	if err != nil {
		return record, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return record, err
	}
	if !info.Mode().IsRegular() || info.Size() > store.maxBytes {
		return record, invalid
	}
	decoder := json.NewDecoder(io.LimitReader(file, store.maxBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, fmt.Errorf("decode %s record: %w", store.label, err)
	}
	if !store.valid(record) {
		return record, errors.New(store.label + " record is invalid")
	}
	return record, nil
}

func (store attemptStore[T]) ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New(store.label + " directory is not a directory")
	}
	return os.Chmod(path, 0o700)
}
