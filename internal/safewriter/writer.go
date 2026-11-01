// Package safewriter serializes writes to non-file output shared by subprocesses.
package safewriter

import (
	"io"
	"os"
	"reflect"
	"sync"
)

type writer struct {
	mu *sync.Mutex
	w  io.Writer
}

func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// Outputs preserves files and already wrapped writers. Non-file outputs passed
// together share a lock, which is never held across a pipe's lifetime.
func Outputs(outputs ...io.Writer) []io.Writer {
	result := make([]io.Writer, len(outputs))
	aliases := make(map[io.Writer]io.Writer)
	mu := &sync.Mutex{}
	for _, output := range outputs {
		if wrapped, ok := output.(*writer); ok && wrapped != nil {
			mu = wrapped.mu
			break
		}
	}
	for i, output := range outputs {
		switch output.(type) {
		case nil, *os.File, *writer:
			result[i] = output
			continue
		}
		comparable := reflect.ValueOf(output).Comparable()
		if comparable {
			if prior, ok := aliases[output]; ok {
				result[i] = prior
				continue
			}
		}
		result[i] = &writer{mu: mu, w: output}
		if comparable {
			aliases[output] = result[i]
		}
	}
	return result
}
