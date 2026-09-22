package sshagentruntime

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"
)

const maxAgentFrame = 256 << 10

// serveFiltered exposes only SSH2 identity enumeration and signing. Every other
// opcode, including extension and legacy requests, fails without reaching the
// private agent. Closing the grant also closes existing protocol connections.
func serveFiltered(ctx context.Context, guest, upstream net.Conn) {
	defer guest.Close()
	defer upstream.Close()
	idle := time.AfterFunc(30*time.Second, func() { guest.Close(); upstream.Close() })
	defer idle.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			guest.Close()
			upstream.Close()
		case <-done:
		}
	}()
	for {
		idle.Reset(30 * time.Second)
		request, err := readFrame(guest)
		if err != nil || ctx.Err() != nil {
			return
		}
		if request[0] != 11 && request[0] != 13 {
			if err := writeFrame(guest, []byte{5}); err != nil {
				return
			}
			continue
		}
		upstream.SetDeadline(time.Now().Add(10 * time.Second))
		if err := writeFrame(upstream, request); err != nil {
			return
		}
		response, err := readFrame(upstream)
		if err != nil || ctx.Err() != nil {
			return
		}
		if err := writeFrame(guest, response); err != nil {
			return
		}
	}
}

func readFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n < 1 || n > maxAgentFrame {
		return nil, errors.New("invalid agent frame length")
	}
	body := make([]byte, n)
	_, err := io.ReadFull(r, body)
	return body, err
}
func writeFrame(w io.Writer, body []byte) error {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}
