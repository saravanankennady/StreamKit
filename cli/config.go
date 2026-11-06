package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	configFile   = "config.yaml"
	sealKeyFile  = "secret.key"
	sealPrefix   = "v1:"
	sealKeyBytes = 32
)

// projectEntry is one project in the config file. A secret is never written in the clear: the file holds either the
// secret sealed with the key described in sealKey, or the name of an environment variable that holds it.
type projectEntry struct {
	Name         string `yaml:"name"`
	URL          string `yaml:"url"`
	APIKey       string `yaml:"api_key"`
	SecretSealed string `yaml:"api_secret_sealed,omitempty"`
	SecretEnv    string `yaml:"api_secret_env,omitempty"`
}

// configData is the config file.
type configData struct {
	DefaultProject string          `yaml:"default_project,omitempty"`
	Projects       []*projectEntry `yaml:"projects"`
}

// store is the config file and where it lives.
type store struct {
	dir  string
	data configData
}

func configDir() (string, error) {
	if v := os.Getenv("STREAMKIT_CONFIG_DIR"); v != "" {
		return v, nil
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "streamkit"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not find the config directory: %w", err)
	}
	return filepath.Join(home, ".config", "streamkit"), nil
}

// checkPrivate refuses a file others can read, the way ssh does. Secrets the file protects could already be gone, and
// saying so is more useful than carrying on.
func checkPrivate(path string, info fs.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s can be read by other users (mode %04o). Fix it with: chmod 600 %s", path, perm, path)
	}
	return nil
}

// loadStore reads the config. A file that does not exist yet is an empty config.
func loadStore() (*store, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	s := &store{dir: dir}
	path := filepath.Join(dir, configFile)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := checkPrivate(path, info); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, &s.data); err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	return s, nil
}

// save writes the config in one step, private from the first byte.
func (s *store) save() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(&s.data)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, configFile+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.dir, configFile))
}

func (s *store) find(name string) *projectEntry {
	for _, p := range s.data.Projects {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// sealKey returns the key that seals secrets in the config file. STREAMKIT_CONFIG_KEY lets it live somewhere else, such
// as a secret manager. Otherwise it is a private file beside the config, created on first use, which keeps secrets out
// of a config file that gets pasted, backed up or committed. It does not stop someone who can already read your files.
func (s *store) sealKey(create bool) ([]byte, error) {
	if v := os.Getenv("STREAMKIT_CONFIG_KEY"); v != "" {
		key, err := decodeSealKey(v)
		if err != nil {
			return nil, fmt.Errorf("STREAMKIT_CONFIG_KEY: %w", err)
		}
		return key, nil
	}
	path := filepath.Join(s.dir, sealKeyFile)
	info, err := os.Stat(path)
	if err == nil {
		if err := checkPrivate(path, info); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		key, err := decodeSealKey(strings.TrimSpace(string(data)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if !create {
		return nil, fmt.Errorf("%s is missing, so the secrets in the config file cannot be read. Add the project again", path)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, sealKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	// O_EXCL: two CLIs starting at once must not overwrite each other's key.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		return nil, err
	}
	return key, nil
}

func decodeSealKey(v string) ([]byte, error) {
	if key, err := hex.DecodeString(v); err == nil && len(key) == sealKeyBytes {
		return key, nil
	}
	if key, err := base64.StdEncoding.DecodeString(v); err == nil && len(key) == sealKeyBytes {
		return key, nil
	}
	return nil, fmt.Errorf("must be %d bytes, as hex or base64", sealKeyBytes)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal encrypts a secret, bound to the project's name so that a sealed value cannot be moved to another entry.
func (s *store) seal(name, secret string) (string, error) {
	key, err := s.sealKey(true)
	if err != nil {
		return "", err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return sealPrefix + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(secret), []byte(name))), nil
}

func (s *store) unseal(name, sealed string) (string, error) {
	encoded, ok := strings.CutPrefix(sealed, sealPrefix)
	if !ok {
		return "", errors.New("the secret is in a form this version does not know")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", errors.New("the secret is damaged")
	}
	key, err := s.sealKey(false)
	if err != nil {
		return "", err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", errors.New("the secret is damaged")
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], []byte(name))
	if err != nil {
		return "", errors.New("the secret could not be opened with this key. If the key changed, add the project again")
	}
	return string(plain), nil
}

// secretOf returns a project's secret, from wherever it is kept.
func (s *store) secretOf(p *projectEntry) (string, error) {
	switch {
	case p.SecretEnv != "":
		v := os.Getenv(p.SecretEnv)
		if v == "" {
			return "", fmt.Errorf("project %q keeps its secret in %s, which is not set", p.Name, p.SecretEnv)
		}
		return v, nil
	case p.SecretSealed != "":
		v, err := s.unseal(p.Name, p.SecretSealed)
		if err != nil {
			return "", fmt.Errorf("project %q: %w", p.Name, err)
		}
		return v, nil
	}
	return "", fmt.Errorf("project %q has no secret", p.Name)
}
