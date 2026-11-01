package nativejson

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
)

// PrivateFrame reuses the existing SYC1 header, sequence, kind15 and digest for
// fixed leaf exchanges. It transports no executable grammar or descriptors.
func WritePrivateFrame(w io.Writer, sequence uint64, raw []byte, limit int) error {
	if sequence == 0 || len(raw) == 0 || len(raw) > limit {
		return errors.New("private native frame bound")
	}
	h := make([]byte, 52)
	copy(h, "SYC1")
	h[4] = 1
	h[5] = 15
	binary.BigEndian.PutUint64(h[8:16], sequence)
	sum := sha256.Sum256(raw)
	copy(h[16:48], sum[:])
	binary.BigEndian.PutUint32(h[48:], uint32(len(raw)))
	for _, part := range [][]byte{h, raw} {
		for len(part) > 0 {
			n, err := w.Write(part)
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			part = part[n:]
		}
	}
	return nil
}
func ReadPrivateFrame(r io.Reader, sequence uint64, limit int) ([]byte, error) {
	h := make([]byte, 52)
	if _, err := io.ReadFull(r, h); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(h[48:])
	if !bytes.Equal(h[:4], []byte("SYC1")) || h[4] != 1 || h[5] != 15 || h[6] != 0 || h[7] != 0 || binary.BigEndian.Uint64(h[8:16]) != sequence || sequence == 0 || n == 0 || uint64(n) > uint64(limit) {
		return nil, errors.New("private native frame refusal")
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(r, raw); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	if !bytes.Equal(h[16:48], sum[:]) {
		return nil, errors.New("private native frame digest")
	}
	return raw, nil
}
