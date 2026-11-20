// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

const encPrefix = "enc:v1:"

// loadKey reads the 32 byte key from VOLTIBRI_SECRET_KEY (base64) or the
// key file, creating a new random key file (mode 0600) if neither exists.
func loadKey(path string) ([]byte, error) {
	if env := os.Getenv("VOLTIBRI_SECRET_KEY"); env != "" {
		return decodeKey(env, "VOLTIBRI_SECRET_KEY")
	}
	b, err := os.ReadFile(path)
	if err == nil {
		return decodeKey(strings.TrimSpace(string(b)), path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func decodeKey(s, source string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s: expected a base64 encoded 32 byte key", source)
	}
	return key, nil
}

func (s *Store) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Store) encrypt(plain string) (string, error) {
	if strings.HasPrefix(plain, encPrefix) {
		return plain, nil // already encrypted
	}
	g, err := s.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return encPrefix + base64.StdEncoding.EncodeToString(g.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (s *Store) decrypt(v string) (string, error) {
	if !strings.HasPrefix(v, encPrefix) {
		return v, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, encPrefix))
	if err != nil {
		return "", err
	}
	g, err := s.aead()
	if err != nil {
		return "", err
	}
	if len(raw) < g.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	plain, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], nil)
	if err != nil {
		return "", errors.New("cannot decrypt secret (wrong secret.key?)")
	}
	return string(plain), nil
}
