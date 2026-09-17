package mfa

import (
	"crypto/rand"
	"fmt"
)

func generateRandomBytes(buf []byte) (int, error) {
	n, err := rand.Read(buf)
	if err != nil {
		return 0, fmt.Errorf("read random bytes: %w", err)
	}
	return n, nil
}
