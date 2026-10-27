package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	keyVar     = "PSBIN_KEY"
	sealPrefix = "enc1:"
)

var errNoKey = errors.New("no key")

// dataDir is where the paste file and .env live: $PSBIN_DIR, else psbin/ in
// the user config dir. It's fixed per user (not the working directory), so
// the server and the CLI always find the same data.
func dataDir() (string, error) {
	if d := os.Getenv("PSBIN_DIR"); d != "" {
		return d, nil
	}
	c, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(c, "psbin"), nil
}

func parseKey(s string) ([]byte, error) {
	k, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(k) != 32 {
		return nil, fmt.Errorf("%s must be 64 hex characters", keyVar)
	}
	return k, nil
}

// loadKey reads the key from $PSBIN_KEY, else from dir/.env. It returns
// errNoKey if there is none.
func loadKey(dir string) ([]byte, error) {
	if v := os.Getenv(keyVar); v != "" {
		return parseKey(v)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".env"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoKey
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), keyVar+"="); ok {
			return parseKey(strings.Trim(v, `"'`))
		}
	}
	return nil, errNoKey
}

// newKey makes a fresh key and appends it to dir/.env.
func newKey(dir string) ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".env")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s=%s\n", keyVar, hex.EncodeToString(key)); err != nil {
		return nil, err
	}
	slog.Info("generated encryption key", "path", path)
	return key, nil
}

// sealed encrypts paste content on its way into a Store and decrypts it on
// the way out, so the store (file or database) only ever holds ciphertext.
type sealed struct {
	Store
	aead cipher.AEAD
}

func newSealed(inner Store, key []byte) (*sealed, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sealed{Store: inner, aead: aead}, nil
}

// seal returns "enc1:" + base64(nonce || ciphertext). The id is authenticated
// but not encrypted, so a ciphertext can't be moved to another paste's row.
func (s *sealed) seal(id, content string) string {
	nonce := make([]byte, s.aead.NonceSize())
	rand.Read(nonce)
	return sealPrefix + base64.StdEncoding.EncodeToString(
		s.aead.Seal(nonce, nonce, []byte(content), []byte(id)))
}

func (s *sealed) open(id, stored string) (string, error) {
	enc, ok := strings.CutPrefix(stored, sealPrefix)
	if !ok {
		return stored, nil // written before encryption existed
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return "", errors.New("corrupt ciphertext")
	}
	n := s.aead.NonceSize()
	pt, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(id))
	if err != nil {
		return "", errors.New("cannot decrypt (wrong key?)")
	}
	return string(pt), nil
}

func (s *sealed) Create(ctx context.Context, id, content string) error {
	return s.Store.Create(ctx, id, s.seal(id, content))
}

func (s *sealed) Get(ctx context.Context, id string) (paste, error) {
	p, err := s.Store.Get(ctx, id)
	if err != nil {
		return p, err
	}
	p.Content, err = s.open(p.ID, p.Content)
	return p, err
}

// Recent decrypts, then cuts each paste to previewLen.
func (s *sealed) Recent(ctx context.Context, n int) ([]paste, error) {
	ps, err := s.Store.Recent(ctx, n)
	if err != nil {
		return nil, err
	}
	for i := range ps {
		c, err := s.open(ps[i].ID, ps[i].Content)
		if err != nil {
			slog.Warn("unreadable paste", "id", ps[i].ID, "err", err)
			c = "[" + err.Error() + "]"
		}
		ps[i].Content = preview(c)
	}
	return ps, nil
}
