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
type RPCErrorLog struct {
	Home    string
	Maximum int64
}

// Append adds one record, first rotating a log that reached its maximum.
func (log RPCErrorLog) Append(record RPCError) error {
	if log.Home == "" {
		return nil
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
	path := filepath.Join(directory, rpcErrorFile)
	info, err := os.Lstat(path)
	if err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular RPC error log %s", path)
	}
	if err == nil && info.Size() >= maximum {
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
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
	// O_NOFOLLOW keeps a link created after the check from receiving the entry.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(append(line, '\n'))
	return errors.Join(err, file.Close())
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
