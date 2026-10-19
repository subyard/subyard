package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/Subyard/Subyard/internal/config"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type request struct {
	Version int                `json:"version"`
	Op      string             `json:"op"`
	Yard    string             `json:"yard"`
	Recipe  *config.SSHPortPin `json:"recipe"`
	Pause   string             `json:"pause"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "release-owner-native: refused")
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) != 3 {
		return fmt.Errorf("arguments")
	}
	fd, e := strconv.Atoi(os.Args[1])
	if e != nil || fd < 3 {
		return fmt.Errorf("descriptor")
	}
	controlFD, e := strconv.Atoi(os.Args[2])
	if e != nil {
		return e
	}
	var control *os.File
	if controlFD >= 3 && controlFD != fd {
		control = os.NewFile(uintptr(controlFD), "fixture-control")
		defer control.Close()
	}
	pauseAt := func(point string) error {
		if point == "" {
			return nil
		}
		if control == nil {
			return fmt.Errorf("control")
		}
		fmt.Fprintln(os.Stderr, "native-cut:"+point)
		var byte [1]byte
		if _, err := io.ReadFull(control, byte[:]); err != nil || byte[0] != 'c' {
			return fmt.Errorf("control")
		}
		return nil
	}
	root := os.NewFile(uintptr(fd), "borrowed-root")
	defer root.Close()
	actual, e := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if e != nil || filepath.Dir(actual) != "/tmp" || !strings.HasPrefix(filepath.Base(actual), "subyard-owner-") {
		return fmt.Errorf("fixture root")
	}
	payload, e := io.ReadAll(io.LimitReader(os.Stdin, (1<<20)+1))
	if e != nil || len(payload) > 1<<20 {
		return fmt.Errorf("bounds")
	}
	var r request
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&r); e != nil {
		return e
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return fmt.Errorf("trailing")
	}
	encoded, e := canonicalWire(r)
	if e != nil || !bytes.Equal(encoded, payload) {
		return fmt.Errorf("noncanonical request")
	}
	if r.Pause != "" && r.Pause != "before_cas" && r.Pause != "after_cas" {
		return fmt.Errorf("pause")
	}
	if r.Version != 1 {
		return fmt.Errorf("version")
	}
	status := ""
	var recipe *config.SSHPortPin
	switch r.Op {
	case "prepare":
		if r.Recipe != nil {
			return fmt.Errorf("recipe")
		}
		p, err := config.PrepareSSHPortPin(root, r.Yard)
		if err != nil {
			return err
		}
		recipe = &p
		status = "prepared"
	case "observe", "apply":
		if r.Recipe == nil || r.Yard != r.Recipe.Yard {
			return fmt.Errorf("scope")
		}
		if r.Op == "observe" {
			status, e = config.ObserveSSHPortPin(root, *r.Recipe)
		} else {
			if r.Pause == "before_cas" {
				if err := pauseAt(r.Pause); err != nil {
					return err
				}
			}
			status, e = config.ApplySSHPortPin(root, *r.Recipe)
			if r.Pause == "after_cas" {
				if err := pauseAt(r.Pause); err != nil {
					return err
				}
			}
		}
		if e != nil {
			return e
		}
	default:
		return fmt.Errorf("operation")
	}
	response := map[string]any{"version": 1, "status": status, "recipe": recipe}
	b, e := canonicalWire(response)
	if e != nil {
		return e
	}
	if len(b) > 1<<20 {
		return fmt.Errorf("response bound")
	}
	_, e = os.Stdout.Write(b)
	return e
}

func canonicalWire(value any) ([]byte, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var normalized any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if err = decoder.Decode(&normalized); err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}
