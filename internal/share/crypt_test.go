package share

import (
	"strings"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	ct, err := Encrypt("s3cr3t-key", "hunter2")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	pt, err := Decrypt("s3cr3t-key", ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if pt != "hunter2" {
		t.Errorf("Decrypt = %q, want %q", pt, "hunter2")
	}
}

func TestEncryptRejectsEmptyKey(t *testing.T) {
	if _, err := Encrypt("", "hunter2"); err != ErrEmptyKey {
		t.Errorf("Encrypt with empty key = %v, want ErrEmptyKey", err)
	}
}

func TestDecryptRejectsEmptyKey(t *testing.T) {
	if _, err := Decrypt("", []byte("whatever")); err != ErrEmptyKey {
		t.Errorf("Decrypt with empty key = %v, want ErrEmptyKey", err)
	}
}

func TestDecryptDetectsTamper(t *testing.T) {
	ct, err := Encrypt("s3cr3t-key", "hunter2")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0xFF // flip a bit in the auth tag

	if _, err := Decrypt("s3cr3t-key", tampered); err == nil {
		t.Error("Decrypt of tampered ciphertext succeeded, want error")
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	ct, err := Encrypt("key-a", "hunter2")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := Decrypt("key-b", ct); err == nil {
		t.Error("Decrypt with wrong key succeeded, want error")
	}
}

func TestDecryptTooShortFails(t *testing.T) {
	if _, err := Decrypt("s3cr3t-key", []byte("x")); err == nil {
		t.Error("Decrypt of too-short ciphertext succeeded, want error")
	} else if !strings.Contains(err.Error(), "too short") {
		t.Errorf("Decrypt error = %v, want mention of \"too short\"", err)
	}
}
