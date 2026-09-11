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

// ErrKeyUnreadable wraps any failure to decrypt/decode a *stored* dashcam
// private key — wrong or missing DASHCAM_SECRET_KEY (other than empty,
// which is share.ErrEmptyKey instead — see below), or corrupt data — as
// opposed to "no key has ever been saved" (LoadKey's ok=false, nil err
// case). Handlers distinguish the two: GET /api/dashcams/key and POST
// .../key/generate treat this as "configured, but unreadable" (200 with
// error_code:"key_unreadable", still reporting key_id/public_key_b64 from
// the unencrypted settings) rather than a 500, since it's an expected,
// actionable operator mistake (rotated DASHCAM_SECRET_KEY without
// regenerating the dashcam key) rather than a server bug.
var ErrKeyUnreadable = errors.New("dashcam: stored private key could not be decrypted")

// LoadKey reads back what SaveKey wrote, decrypting the private key under
// secret. ok is false (with a nil error) when no key has been saved yet.
//
// keyID is returned even when err != nil and priv is nil, for every
// failure mode from the private-key decode stage onward (i.e. once
// SettingKeyID has been read and decoded successfully) — the key id and
// public key are stored unencrypted, so a caller whose only problem is a
// stale/wrong secret key can still report which key is configured. err is
// share.ErrEmptyKey if secret is "" (share.Decrypt's own sentinel,
// deliberately not wrapped further so errors.Is(err, share.ErrEmptyKey)
// keeps working), or wraps ErrKeyUnreadable for any other decode/decrypt
// failure.
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
		return keyID, nil, false, fmt.Errorf("%w: decode private key: %v", ErrKeyUnreadable, err)
	}
	encrypted, err := base64.StdEncoding.DecodeString(encB64)
	if err != nil {
		return keyID, nil, false, fmt.Errorf("%w: decode private key base64: %v", ErrKeyUnreadable, err)
	}
	seedB64, err := share.Decrypt(secret, encrypted)
	if err != nil {
		if errors.Is(err, share.ErrEmptyKey) {
			return keyID, nil, false, share.ErrEmptyKey
		}
		return keyID, nil, false, fmt.Errorf("%w: %v", ErrKeyUnreadable, err)
	}
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		return keyID, nil, false, fmt.Errorf("%w: decode private key seed: %v", ErrKeyUnreadable, err)
	}
	if len(seed) != ed25519.SeedSize {
		return keyID, nil, false, fmt.Errorf("%w: private key seed has wrong length: %d", ErrKeyUnreadable, len(seed))
	}
	priv = ed25519.NewKeyFromSeed(seed)
	return keyID, priv, true, nil
}

// LoadPubKeyB64 reads back the unencrypted public key setting SaveKey
// wrote (SettingPubKeyB64), for a caller (e.g. GET /api/dashcams/key's
// key_unreadable branch) that needs it without being able to decrypt the
// private key. ok is false (nil error) when no key has ever been saved.
func LoadPubKeyB64(ctx context.Context, store *db.DB) (pubB64 string, ok bool, err error) {
	raw, err := store.SettingsGet(ctx, SettingPubKeyB64)
	if errors.Is(err, db.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("dashcam: load public key: %w", err)
	}
	if err := json.Unmarshal(raw, &pubB64); err != nil {
		return "", false, fmt.Errorf("dashcam: decode public key: %w", err)
	}
	return pubB64, true, nil
}

func mustJSON(v string) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err) // marshaling a plain string can't fail
	}
	return raw
}
