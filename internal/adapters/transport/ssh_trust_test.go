package transport

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSSHTrustPrecedesPayloadAndNetworkTimeout(t *testing.T) {
	for _, decline := range []bool{false, true} {
		input := strings.NewReader("protected payload")
		sentinel := errors.New("declined")
		calls := 0
		ctx := WithSSHTrust(context.Background(), func(ctx context.Context, _ string, _ string) ([]string, error) {
			calls++
			if _, ok := ctx.Deadline(); ok {
				t.Fatal("network deadline started before consent")
			}
			if input.Len() != len("protected payload") {
				t.Fatal("payload consumed before consent")
			}
			if decline {
				return nil, sentinel
			}
			return nil, nil
		})
		process := Process{Program: "cat", SSHTarget: "example", Timeout: 10 * time.Second}
		output, err := process.CallReader(ctx, input)
		if calls != 1 {
			t.Fatalf("gate calls=%d", calls)
		}
		if decline {
			if !errors.Is(err, sentinel) || input.Len() != len("protected payload") {
				t.Fatalf("decline=%v remaining=%d", err, input.Len())
			}
		} else if err != nil || string(output) != "protected payload" {
			t.Fatalf("prompt consumed network deadline: output=%q err=%v", output, err)
		}
	}
}
