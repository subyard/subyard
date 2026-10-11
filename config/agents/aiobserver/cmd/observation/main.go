// AI Observer's package-owned semantic producer. Python owns only transport and
// bounded lifecycle effects; this executable owns native validation and hashing.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	producer "github.com/Subyard/Subyard/config/agents/aiobserver/observation"
	semantic "github.com/Subyard/Subyard/internal/observation"
)

func main() {
	if err := execute(); err != nil {
		fmt.Fprintln(os.Stderr, "subyard-lifecycle-diagnostic-v1 "+producer.Reason(err))
		os.Exit(1)
	}
}
func execute() error {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, semantic.MaxInput+1))
	if err != nil || len(data) > semantic.MaxInput {
		return &producer.Failure{Reason: "invalid-schema"}
	}
	if len(os.Args) != 2 {
		return &producer.Failure{Reason: "invalid-schema"}
	}
	var result any
	switch os.Args[1] {
	case "container":
		result, err = producer.Container(data)
	case "observe":
		// Validate the envelope too: duplicate keys and lossy Unicode are refused.
		if _, err = semantic.Decode(data); err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		var request producer.Request
		if err = decoder.Decode(&request); err != nil {
			return err
		}
		result, err = producer.Observe(request)
	default:
		return &producer.Failure{Reason: "invalid-schema"}
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
