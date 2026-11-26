package audit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/safefs"
)

const (
	rpcErrorFile           = "rpc-errors.log"
	defaultRPCErrorMaximum = int64(256 * 1024)
	maxRPCErrorText        = 8 * 1024
)

// RPCError is one failed Yard RPC request served by this owner. Error is the
// full local error text, which the owner's operator may read; the log is
// private to the owner.
type RPCError struct {
	At        time.Time `json:"at"`
	Request   string    `json:"request"`
	Operation string    `json:"operation,omitempty"`
	Method    string    `json:"method"`
	Code      string    `json:"code"`
	Error     string    `json:"error"`
}

// RPCErrorLog keeps failed RPC requests in logs/rpc-errors.log under the
// Subyard data home, with one previous generation, each about Maximum bytes.
// BeforeWrite admits each write like an Invocation's, so a late session
// neither recreates a removed installation nor enters a new one.
type RPCErrorLog struct {
	Home        string
	Maximum     int64
	BeforeWrite func(context.Context) (func(), bool, error)
}

// Append adds one record, first rotating a log that reached its maximum.
func (log RPCErrorLog) Append(record RPCError) error {
	if log.Home == "" {
		return nil
	}
	if log.BeforeWrite != nil {
		release, write, err := log.BeforeWrite(context.Background())
		if err != nil || !write {
			return err
		}
		defer release()
	}
	directory := filepath.Join(log.Home, "logs")
	if err := safefs.AdmitDir(directory, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	lock, err := safefs.Lock(context.Background(), filepath.Join(directory, ".yard.lock"), false, 0)
	if err != nil {
		return err
	}
	defer lock.Release()
	maximum := log.Maximum
	if maximum <= 0 {
		maximum = defaultRPCErrorMaximum
	}
	if len(record.Error) > maxRPCErrorText {
		record.Error = record.Error[:maxRPCErrorText] + "…"
	}
	if record.At.IsZero() {
		record.At = time.Now()
	}
	record.At = record.At.UTC()
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	path := filepath.Join(directory, rpcErrorFile)
	file, err := openRPCErrorLog(path)
	if err != nil {
		return err
	}
	if info, err := file.Stat(); err != nil || info.Size() >= maximum {
		// The full log, already private, becomes the previous generation.
		_ = file.Close()
		if err != nil {
			return err
		}
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
		if file, err = openRPCErrorLog(path); err != nil {
			return err
		}
	}
	_, err = file.Write(append(line, '\n'))
	return errors.Join(err, file.Close())
}

// openRPCErrorLog opens the regular log for appending and narrows a file that
// an earlier writer left readable by others.
func openRPCErrorLog(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing non-regular RPC error log %s", path)
	}
	// O_NOFOLLOW keeps a link created after the check from receiving the entry.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := safefs.RepairMode(file, 0o077); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

// Read returns the newest records, oldest first, at most limit of them.
func (log RPCErrorLog) Read(limit int) ([]RPCError, error) {
	if log.Home == "" || limit < 1 {
		return nil, errors.New("RPC error log home and a positive limit are required")
	}
	records := []RPCError{}
	path := filepath.Join(log.Home, "logs", rpcErrorFile)
	for _, name := range []string{path + ".1", path} {
		info, err := os.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("RPC error log is not a regular file")
		}
		payload, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(bytes.NewReader(payload))
		scanner.Buffer(make([]byte, 0, 64*1024), 2*maxRPCErrorText+4096)
		for scanner.Scan() {
			var record RPCError
			if json.Unmarshal(scanner.Bytes(), &record) == nil {
				records = append(records, record)
			}
		}
	}
	if len(records) > limit {
		records = records[len(records)-limit:]
	}
	return records, nil
}
