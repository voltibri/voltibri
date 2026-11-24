// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (OWASP: m=19 MiB, t=2, p=1 is the minimum; a little
// more here, a Raspberry Pi still needs well below a second).
const (
	argonTime    = 3
	argonMemory  = 32 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

// MinPasswordLen is the minimum password length.
const MinPasswordLen = 10

// CheckPasswordRules returns an error for passwords that are too weak.
func CheckPasswordRules(pw string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLen {
		return fmt.Errorf("Das Passwort muss mindestens %d Zeichen lang sein.", MinPasswordLen)
	}
	if utf8.RuneCountInString(pw) > 256 {
		return errors.New("Das Passwort ist zu lang (höchstens 256 Zeichen).")
	}
	return nil
}

// HashPassword returns an argon2id hash in PHC format.
func HashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// dummyHash makes a login with an unknown user name take as long as one
// with a wrong password.
var dummyHash = HashPassword("voltibri-timing-equaliser")

// VerifyPassword checks a password against a hash ("" never matches).
func VerifyPassword(hash, pw string) bool {
	if hash == "" {
		VerifyPassword(dummyHash, pw)
		return false
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err1 := b64.DecodeString(parts[4])
	want, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
