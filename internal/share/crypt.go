// Package share owns SMB/CIFS share configuration: encrypting/decrypting
// stored credentials (crypt.go), mounting/unmounting via mount.cifs
// (mount.go), and a background health loop that keeps shares.status
// current (health.go).
package share

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// ErrEmptyKey is returned by Encrypt (and surfaced by handlers as a 400)
// when DASHCAM_SECRET_KEY is unset — an empty key would mean stored
// SMB/CIFS passwords can't be meaningfully encrypted.
var ErrEmptyKey = errors.New("share: DASHCAM_SECRET_KEY is empty; set it before configuring SMB/CIFS shares")

// deriveKey turns the operator-supplied secret key string into a 32-byte
// AES-256 key via SHA-256, so any non-empty string (not just a 32-byte
// value) is usable as DASHCAM_SECRET_KEY.
func deriveKey(secretKey string) [32]byte {
	return sha256.Sum256([]byte(secretKey))
}

// Encrypt AES-256-GCM-encrypts plaintext using a key derived from
// secretKey, returning nonce||ciphertext||tag. Returns ErrEmptyKey if
// secretKey is empty.
func Encrypt(secretKey, plaintext string) ([]byte, error) {
	if secretKey == "" {
		return nil, ErrEmptyKey
	}
	key := deriveKey(secretKey)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("share: crypt: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("share: crypt: new gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("share: crypt: nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Decrypt reverses Encrypt. Returns an error (tamper detection) if data was
// truncated, encrypted under a different key, or modified since sealing.
func Decrypt(secretKey string, data []byte) (string, error) {
	if secretKey == "" {
		return "", ErrEmptyKey
	}
	key := deriveKey(secretKey)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmt.Errorf("share: crypt: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("share: crypt: new gcm: %w", err)
	}
	if len(data) < gcm.NonceSize() {
		return "", errors.New("share: crypt: ciphertext too short")
	}
	nonce, ct := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("share: crypt: decrypt: %w", err)
	}
	return string(plaintext), nil
}
