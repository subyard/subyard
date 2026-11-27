package collector

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func RunCLI(args []string, receiver ReceiverFunc) error {
	if len(args) == 0 {
		return errors.New("usage: subyard-telemetry prepare|serve|export|replay|prune|show-run|stats|query|sanitize [options]")
	}
	command := args[0]
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	family := f.String("family", "record", "record or state family to sanitize from stdin")
	origin := f.String("origin", "", "store origin for show-run (defaults to local, then a unique imported run)")
	dir := f.String("data-dir", "", "private telemetry directory")
	listen := f.String("listen", "127.0.0.1:4318", "loopback OTLP HTTP listener")
	auth := f.String("auth-file", "", "private bearer token file")
	spool := f.String("spool", "", "private data-dir/spool")

	policyFile := f.String("policy-file", "", "capture policy JSON")
	after := f.Int64("after", 0, "exclusive export sequence cursor")
	limit := f.Int("limit", 1000, "maximum export rows per read window")
	runID := f.String("run", "", "agent run identifier")
	includeChildren := f.Bool("include-children", false, "include explicitly related descendant runs in show-run")
	input := f.String("input", "-", "replay JSONL file or -")
	query := f.String("sql", "SELECT * FROM sample_values LIMIT 100", "read-only SQL")
	disk := f.Int64("disk-bytes", DefaultDiskBytes, "soft telemetry disk budget; above it the oldest raw records older than 30 days are pruned")
	queue := f.Int64("queue-bytes", 16<<20, "in-memory queued sanitized bytes")
	requests := f.Int("queue-requests", 128, "maximum queued requests")
	olderThan := f.String("older-than", "", "prune records received before this age, such as 60d or 1440h")
	includeUsage := f.Bool("include-usage", false, "prune also usage records and their bindings")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if command == "sanitize" {
		return runSanitizer(*family, os.Stdin, os.Stdout)
	}
	if *dir == "" {
		return errors.New("--data-dir is required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if command == "prune" {
		return runPrune(ctx, os.Stdout, *dir, *olderThan, *includeUsage, *disk)
	}
	if command == "serve" || command == "replay" || command == "prepare" {
		policy, err := LoadPolicy(*policyFile)
		if err != nil {
			return err
		}
		limits := DefaultLimits()
		limits.DiskBytes = *disk
		limits.QueueBytes = *queue
		limits.QueueRequests = *requests
		store, err := openStore(*dir, limits)
		if err != nil {
			return err
		}
		if command == "prepare" {
			return errors.Join(store.publishCapturePolicy(ctx, policy), store.Close())
		}
		defer store.Close()
		if command == "serve" {
			return Serve(ctx, store, Options{Listen: *listen, AuthFile: *auth, Spool: *spool, Receiver: receiver, Policy: policy})
		}
		var reader io.Reader = os.Stdin
		if *input != "-" {
			file, err := os.Open(*input)
			if err != nil {
				return err
			}
			defer file.Close()
			reader = file
		}
		return store.replay(ctx, reader, policy)
	}
	db, err := OpenReadOnly(*dir)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, deadline := context.WithTimeout(ctx, 5*time.Second)
	defer deadline()
	var result any
	switch command {
	case "export":
		_, err = Export(ctx, db, os.Stdout, *after, *limit)
		return err
	case "show-run":
		if *runID == "" {
			return errors.New("--run is required")
		}
		result, err = showRunOrigin(ctx, db, *runID, *origin, *includeChildren)
	case "stats":
		var stats map[string]any
		if stats, err = Stats(ctx, db); err == nil {
			addStorageWarning(stats, *dir)
		}
		result = stats
	case "query":
		result, err = Query(ctx, db, *query)
	default:
		return errors.New("unknown telemetry command")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func runSanitizer(family string, reader io.Reader, writer io.Writer) error {
	const limit = 2 << 20
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || len(data) > limit {
		return errInvalidRecord
	}
	input, err := decodeBody(data)
	if err != nil || !json.Valid(data) {
		return errInvalidRecord
	}
	encoder := json.NewEncoder(writer)
	if family != "record" {
		clean, err := sanitizeState(family, input)
		if err != nil {
			return err
		}
		return encoder.Encode(clean)
	}
	var command struct {
		Record Record `json:"record"`
		Policy Policy `json:"policy"`
	}
	if err := json.Unmarshal(data, &command); err != nil || command.Policy.Schema != PolicyVersion {
		return errInvalidRecord
	}
	clean, err := sanitizeRecord(command.Record, command.Policy, command.Record.filter)
	if err != nil {
		return errInvalidRecord
	}
	return encoder.Encode(clean)
}

// runPrune asks the running collector over its control socket and opens the
// store itself only when no collector holds it.
func runPrune(ctx context.Context, out io.Writer, dir, olderThan string, includeUsage bool, disk int64) error {
	age, err := parseAge(olderThan)
	if err != nil {
		return err
	}
	removed, err := pruneViaControl(ctx, dir, age, includeUsage)
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		limits := DefaultLimits()
		limits.DiskBytes = disk
		var store *Store
		if store, err = openStore(dir, limits); err != nil {
			return err
		}
		removed, err = store.Prune(time.Now().Add(-age), includeUsage)
		err = errors.Join(err, store.Close())
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"removed_records": removed})
}

// addStorageWarning reports the collector's space markers; the cleanup
// command names this binary and data directory, so it runs as printed.
func addStorageWarning(stats map[string]any, dir string) {
	var warnings []string
	if since, found := readSpaceMarker(dir, OverBudgetMarker, "over_budget_since="); found {
		stats["over_budget_since"] = since
		warnings = append(warnings, "store above its disk budget since "+since+" with only records younger than 30 days; it keeps growing. Raise TELEMETRY_DISK_BUDGET or remove recent data with prune --older-than")
	}
	if since, found := readSpaceMarker(dir, RawRefusedMarker, "raw_otlp_refused_since="); found {
		stats["raw_otlp_refused_since"] = since
		binary, err := os.Executable()
		if err != nil {
			binary = "subyard-telemetry"
		}
		if absolute, err := filepath.Abs(dir); err == nil {
			dir = absolute
		}
		warnings = append(warnings, "raw OTLP refused since "+since+": data partition reserve reached; usage is still recorded. Free space with: "+
			shellWord(binary)+" prune --data-dir "+shellWord(dir)+" --older-than 60d")
	}
	if warnings != nil {
		stats["warnings"] = warnings
	}
}

func readSpaceMarker(dir, name, prefix string) (string, bool) {
	marker, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", false
	}
	return strings.CutPrefix(strings.TrimSpace(string(marker)), prefix)
}

func shellWord(value string) string {
	if value != "" && strings.Trim(value, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-") == "" {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func parseAge(value string) (time.Duration, error) {
	age, err := time.ParseDuration(value)
	if days, ok := strings.CutSuffix(value, "d"); ok {
		var n int
		n, err = strconv.Atoi(days)
		age = time.Duration(n) * 24 * time.Hour
	}
	if err != nil || age <= 0 {
		return 0, errors.New("--older-than requires a positive age such as 60d or 1440h")
	}
	return age, nil
}

func pruneViaControl(ctx context.Context, dir string, age time.Duration, includeUsage bool) (int64, error) {
	client := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "control.sock"))
	}}}
	query := url.Values{"older_than": {age.String()}}
	if includeUsage {
		query.Set("include_usage", "1")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://control/prune?"+query.Encode(), nil)
	if err != nil {
		return 0, err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	var result struct {
		Removed int64 `json:"removed_records"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&result) != nil {
		return 0, errors.New("telemetry prune failed")
	}
	return result.Removed, nil
}
