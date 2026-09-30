package projectruntime

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/Subyard/Subyard/internal/adapters/transport"
)

// VSCode checks the controller listener before opening the preview SSH session.
type VSCode struct{ transport.Process }

func (code VSCode) Run(ctx context.Context, arguments ...string) ([]byte, error) {
	if err := previewPortAvailable("127.0.0.1:8765"); err != nil {
		return nil, err
	}
	return code.Process.Run(ctx, arguments...)
}

func previewPortAvailable(address string) error {
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return fmt.Errorf("preview port %s is unavailable; close its current listener before opening yard code", address)
	}
	if err := listener.Close(); err != nil {
		return errors.New("could not release the preview port before opening VS Code")
	}
	return nil
}
