// Command registry-fixture derives release-transition fixtures with the
// engine's own registry code, so tests never reimplement its digests.
//
//	registry-fixture retire REGISTRY DOMAIN=EPOCH...  prints REGISTRY with those prefixes retired
//	registry-fixture declare REGISTRY                 prints a rollback declaration naming REGISTRY
//
// Both refuse an input the engine cannot parse, and retire refuses a
// retirement that leaves the registry unchanged.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func main() {
	output, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "registry-fixture: %v\n", err)
		os.Exit(2)
	}
	fmt.Println(string(output))
}

func run(arguments []string) ([]byte, error) {
	if len(arguments) < 2 {
		return nil, errors.New("usage: retire REGISTRY DOMAIN=EPOCH... | declare REGISTRY")
	}
	payload, err := os.ReadFile(arguments[1])
	if err != nil {
		return nil, err
	}
	registry, _, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		return nil, err
	}
	digest, err := registry.CanonicalDigest()
	if err != nil {
		return nil, err
	}
	switch {
	case arguments[0] == "declare" && len(arguments) == 2:
		return json.Marshal(releasetransition.RollbackDeclaration{
			SchemaVersion: releasetransition.RollbackDeclarationSchemaV1, PredecessorRegistryDigest: digest,
			Migrations: []json.RawMessage{}, ProfileFormats: []json.RawMessage{},
		})
	case arguments[0] == "retire" && len(arguments) > 2:
		floors := map[string]int{}
		for _, argument := range arguments[2:] {
			domain, value, found := strings.Cut(argument, "=")
			epoch, err := strconv.Atoi(value)
			if !found || err != nil {
				return nil, fmt.Errorf("invalid retirement %q", argument)
			}
			floors[domain] = epoch
		}
		retired, err := registry.RetirePrefixes(floors)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(retired)
		if err != nil {
			return nil, err
		}
		parsed, _, err := releasetransition.ParseRegistryV2(encoded, releasetransition.BuiltinCapabilityCatalog())
		if err != nil {
			return nil, fmt.Errorf("retired registry: %w", err)
		}
		if retiredDigest, err := parsed.CanonicalDigest(); err != nil || retiredDigest == digest {
			return nil, errors.New("the retirement leaves the registry unchanged")
		}
		return encoded, nil
	}
	return nil, errors.New("usage: retire REGISTRY DOMAIN=EPOCH... | declare REGISTRY")
}
