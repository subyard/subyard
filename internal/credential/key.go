package credential

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
)

// ValidateRSAPrivateKey validates a bounded PEM without exposing its contents.
func ValidateRSAPrivateKey(data []byte) error {
	if len(data) > 128<<10 {
		return errors.New("RSA private key is too large")
	}
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("invalid RSA private key")
	}
	var key *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = parsed
	} else if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return errors.New("private key is not RSA")
		}
	} else {
		return errors.New("invalid RSA private key")
	}
	if key.N == nil || key.N.BitLen() < 2048 {
		return errors.New("RSA private key is too small")
	}
	if err := key.Validate(); err != nil {
		return errors.New("invalid RSA private key")
	}
	return nil
}
