package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"time"
)

const generation = 1

type rpcError struct {
	Data struct{ Kind string }
}

func (err *rpcError) Error() string { return "Core rejected the RPC (" + err.Data.Kind + ")" }

func call(socket, method string, params, result any) error {
	conn, err := net.DialTimeout("unix", socket, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	if params == nil {
		params = struct{}{}
	}
	request := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return err
	}
	lines := bufio.NewScanner(conn)
	lines.Buffer(make([]byte, 4096), 4<<20)
	if !lines.Scan() {
		err := lines.Err()
		if err == nil {
			err = io.EOF
		}
		return fmt.Errorf("no reply to %s: %w", method, err)
	}
	var reply struct {
		JSONRPC string
		ID      int
		Error   *rpcError
		Result  json.RawMessage
	}
	if err := json.Unmarshal(lines.Bytes(), &reply); err != nil || reply.JSONRPC != "2.0" || reply.ID != 1 {
		return fmt.Errorf("invalid Core response to %s", method)
	}
	if reply.Error != nil {
		return reply.Error
	}
	if len(reply.Result) == 0 || bytes.Equal(reply.Result, []byte("null")) {
		return fmt.Errorf("empty Core result for %s", method)
	}
	if result != nil {
		if err := json.Unmarshal(reply.Result, result); err != nil {
			return fmt.Errorf("invalid Core result for %s", method)
		}
	}
	return nil
}

type coreStatus struct {
	Core       struct{ ID string }
	PID, Live  int
	Version    string
	CleanStart string
	Holders    []struct {
		PID, Terminals int
		Current        bool
	}
}

func readCore(socket string) (*coreStatus, error) {
	var hello struct{ Generation int }
	err := call(socket, "hello", nil, &hello)
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("inspect Core socket: %w", err)
	case hello.Generation != generation:
		return nil, fmt.Errorf("unsupported Core protocol generation %d", hello.Generation)
	}
	var status coreStatus
	if err := call(socket, "server.status", nil, &status); err != nil {
		return nil, fmt.Errorf("inspect Core status: %w", err)
	}
	if status.PID <= 0 || status.Core.ID == "" || status.Version == "" {
		return nil, errors.New("Core returned an incomplete status")
	}
	status.CleanStart = "unavailable"
	if environment, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", status.PID)); err == nil {
		status.CleanStart = "absent"
		for _, entry := range bytes.Split(environment, []byte{0}) {
			if bytes.Equal(entry, []byte("SUBYARD_HERDR_CLEAN_START=1")) {
				status.CleanStart = "yes"
				break
			}
		}
	}
	return &status, nil
}
