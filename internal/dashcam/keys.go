package dashcam

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"dashcam/internal/db"
	"dashcam/internal/share"
)

// Settings keys this package owns. dashcam_privkey_enc stores
// share.Encrypt(secret, base64(seed)) — the private key's 32-byte seed,
// base64'd and then AES-GCM sealed the same way SMB/CIFS passwords are
// (internal/share/crypt.go), so the manager's secret key protects it too.
const (
	SettingKeyID      = "dashcam_key_id"
	SettingPubKeyB64  = "dashcam_pubkey_b64"
	SettingPrivKeyEnc = "dashcam_privkey_enc"
)

// GenerateKey creates a fresh Ed25519 keypair and a manager-side key id
// ("manager-" + 8 random hex chars) to hand the recorder operator, who
// pastes PublicKeyB64(pub) into the recorder's ingest.public_keys config
// under that same id.
func GenerateKey() (keyID string, pub ed25519.PublicKey, priv ed25519.PrivateKey, err error) {
	pub, priv, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, nil, fmt.Errorf("dashcam: generate key: %w", err)
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", nil, nil, fmt.Errorf("dashcam: generate key id: %w", err)
	}
	keyID = "manager-" + hex.EncodeToString(suffix)
	return keyID, pub, priv, nil
}

// PublicKeyB64 renders a public key as the raw-32-bytes standard base64
// string the recorder's ingest.public_keys config expects.
func PublicKeyB64(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// SaveKey persists keyID/priv to settings, encrypting the private key's
// seed under secret (see the package doc comment above for the encoding).
func SaveKey(ctx context.Context, store *db.DB, secret, keyID string, priv ed25519.PrivateKey) error {
	seed := priv.Seed()
	seedB64 := base64.StdEncoding.EncodeToString(seed)
	encrypted, err := share.Encrypt(secret, seedB64)
	if err != nil {
		return fmt.Errorf("dashcam: encrypt private key: %w", err)
	}
	encB64 := base64.StdEncoding.EncodeToString(encrypted)

	pub := priv.Public().(ed25519.PublicKey)

	values := map[string]json.RawMessage{
		SettingKeyID:      mustJSON(keyID),
		SettingPubKeyB64:  mustJSON(PublicKeyB64(pub)),
		SettingPrivKeyEnc: mustJSON(encB64),
	}
	if err := store.SettingsSetMany(ctx, values); err != nil {
		return fmt.Errorf("dashcam: save key: %w", err)
	}
	return nil
}

// LoadKey reads back what SaveKey wrote, decrypting the private key under
// secret. ok is false (with a nil error) when no key has been saved yet.
func LoadKey(ctx context.Context, store *db.DB, secret string) (keyID string, priv ed25519.PrivateKey, ok bool, err error) {
	keyIDRaw, err := store.SettingsGet(ctx, SettingKeyID)
	if errors.Is(err, db.ErrNotFound) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("dashcam: load key id: %w", err)
	}
	encRaw, err := store.SettingsGet(ctx, SettingPrivKeyEnc)
	if errors.Is(err, db.ErrNotFound) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("dashcam: load private key: %w", err)
	}

	if err := json.Unmarshal(keyIDRaw, &keyID); err != nil {
		return "", nil, false, fmt.Errorf("dashcam: decode key id: %w", err)
	}
	var encB64 string
	if err := json.Unmarshal(encRaw, &encB64); err != nil {
		return "", nil, false, fmt.Errorf("dashcam: decode private key: %w", err)
	}
	encrypted, err := base64.StdEncoding.DecodeString(encB64)
	if err != nil {
		return "", nil, false, fmt.Errorf("dashcam: decode private key base64: %w", err)
	}
	seedB64, err := share.Decrypt(secret, encrypted)
	if err != nil {
		return "", nil, false, fmt.Errorf("dashcam: decrypt private key: %w", err)
	}
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		return "", nil, false, fmt.Errorf("dashcam: decode private key seed: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return "", nil, false, fmt.Errorf("dashcam: private key seed has wrong length: %d", len(seed))
	}
	priv = ed25519.NewKeyFromSeed(seed)
	return keyID, priv, true, nil
}

func mustJSON(v string) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err) // marshaling a plain string can't fail
	}
	return raw
}
