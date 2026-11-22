// rollback-inventory is shared by the repository gate and baseline updater.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/testkit/integrationassets"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) != 3 {
		return fmt.Errorf("usage: rollback-inventory ROOT TRACKED_FILES_JSON")
	}
	root, err := filepath.Abs(os.Args[1])
	if err != nil {
		return err
	}
	data, err := os.ReadFile(os.Args[2])
	if err != nil {
		return err
	}
	var files []string
	if err := json.Unmarshal(data, &files); err != nil {
		return err
	}
	digest, sources, err := integrationassets.Digest(root, files)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Digest  string   `json:"digest"`
		Sources []string `json:"sources"`
	}{digest, sources})
}
