package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Sealer encrypts secrets at rest with AES-256-GCM. The key lives outside the database, so a database
// leak alone does not expose API secrets.
type Sealer struct{ gcm cipher.AEAD }

// NewSealer takes a 32-byte key, encoded as hex or base64.
func NewSealer(encoded string) (*Sealer, error) {
	key, err := decodeKey(encoded)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{gcm: gcm}, nil
}

func decodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, errors.New("master key must be 32 bytes, encoded as hex or base64 (try: openssl rand -hex 32)")
}

// Seal encrypts a value. The nonce is stored at the front of the result.
func (s *Sealer) Seal(plain string) ([]byte, error) {
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.gcm.Seal(nonce, nonce, []byte(plain), nil), nil
}

// Open decrypts a value from Seal.
func (s *Sealer) Open(sealed []byte) (string, error) {
	n := s.gcm.NonceSize()
	if len(sealed) < n {
		return "", errors.New("sealed value is too short")
	}
	plain, err := s.gcm.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// RandomToken returns n random bytes, base64url encoded.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken hashes a high-entropy token for lookup. Such tokens need no slow hash: they cannot be
// guessed from a word list the way passwords can.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Argon2id parameters: OWASP's minimum recommendation at the time of writing.
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword returns an encoded Argon2id hash.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword verifies a password in constant time, using the parameters stored in the hash so they
// can be raised later without invalidating old hashes.
func CheckPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, timeCost, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
