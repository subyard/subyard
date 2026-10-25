package collector

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func RunCLI(args []string, receiver ReceiverFunc) error {
	if len(args) == 0 {
		return errors.New("usage: subyard-telemetry serve|export|replay|show-run|stats|query [options]")
	}
	command := args[0]
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	dir := f.String("data-dir", "", "private telemetry directory")
	listen := f.String("listen", "127.0.0.1:4318", "loopback OTLP HTTP listener")
	auth := f.String("auth-file", "", "private bearer token file")
	spool := f.String("spool", "", "private data-dir/outbox")

	policyFile := f.String("policy-file", "", "capture policy JSON")
	after := f.Int64("after", 0, "exclusive export sequence cursor")
	limit := f.Int("limit", 1000, "maximum export rows per read window")
	runID := f.String("run", "", "agent run identifier")
	includeChildren := f.Bool("include-children", false, "include explicitly related descendant runs in show-run")
	input := f.String("input", "-", "replay JSONL file or -")
	query := f.String("sql", "SELECT * FROM sample_values LIMIT 100", "read-only SQL")
	disk := f.Int64("disk-bytes", 1<<30, "database/WAL capacity ceiling")
	queue := f.Int64("queue-bytes", 16<<20, "in-memory queued sanitized bytes")
	requests := f.Int("queue-requests", 128, "maximum queued requests")
	retention := f.Duration("retention", 7*24*time.Hour, "retention age")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("--data-dir is required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if command == "serve" || command == "replay" {
		limits := DefaultLimits()
		limits.DiskBytes = *disk
		limits.QueueBytes = *queue
		limits.QueueRequests = *requests
		limits.Retention = *retention
		store, err := Open(*dir, limits)
		if err != nil {
			return err
		}
		defer store.Close()
		if command == "serve" {
			policy, err := LoadPolicy(*policyFile)
			if err != nil {
				return err
			}
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
		return store.Replay(ctx, reader)
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
		if *includeChildren {
			result, err = ShowRunIncludingChildren(ctx, db, *runID)
		} else {
			result, err = ShowRun(ctx, db, *runID)
		}
	case "stats":
		result, err = Stats(ctx, db)
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
