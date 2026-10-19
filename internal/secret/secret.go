// Package secret encrypts connector credentials at rest (AES-256-GCM) and
// derives the other keys Fundarr needs from one master key.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Box seals and opens values with a key derived from the master key.
type Box struct {
	aead   cipher.AEAD
	master []byte
}

// New builds a box from a 32-byte master key.
func New(master []byte) (*Box, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(master))
	}
	block, err := aes.NewCipher(Derive(master, "fundarr/secrets"))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, master: master}, nil
}

// Derive returns a 32-byte key for one purpose (HMAC-SHA256 of the label).
func Derive(master []byte, label string) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte(label))
	return m.Sum(nil)
}

// Key derives a purpose-specific key from this box's master key.
func (b *Box) Key(label string) []byte { return Derive(b.master, label) }

// Seal encrypts plaintext; the nonce is prepended to the result.
func (b *Box) Seal(plaintext []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return b.aead.Seal(nonce, nonce, plaintext, nil)
}

var ErrCorrupt = errors.New("secret: cannot decrypt (wrong master key or damaged data)")

// Open decrypts what Seal produced.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, ErrCorrupt
	}
	out, err := b.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return nil, ErrCorrupt
	}
	return out, nil
}

// LoadMaster returns the master key: from env (base64), or from the key file
// in dataDir, which is created on first start. Losing it makes stored
// credentials unreadable, so the file is written once and never replaced.
func LoadMaster(env, dataDir string) ([]byte, bool, error) {
	if env = strings.TrimSpace(env); env != "" {
		k, err := base64.StdEncoding.DecodeString(env)
		if err != nil || len(k) != 32 {
			return nil, false, errors.New("FUNDARR_SECRET_KEY must be 32 bytes in base64 (openssl rand -base64 32)")
		}
		return k, false, nil
	}
	path := filepath.Join(dataDir, "secret.key")
	if raw, err := os.ReadFile(path); err == nil {
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(k) != 32 {
			return nil, false, fmt.Errorf("%s is not a valid key", path)
		}
		return k, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(k) + "\n"); err != nil {
		return nil, false, err
	}
	return k, true, nil
}
