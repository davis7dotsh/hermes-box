package keychain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"filippo.io/age"
)

var (
	ErrNotFound    = errors.New("keychain item not found")
	ErrUnavailable = errors.New("macOS Keychain unavailable")
)

type Store interface {
	Get(account string) ([]byte, error)
	Put(account string, secret []byte) error
	Create(account string, secret []byte) (bool, error)
	Delete(account string) error
}

type MemoryStore struct {
	mu      sync.RWMutex
	secrets map[string][]byte
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{secrets: make(map[string][]byte)}
}

func (s *MemoryStore) Get(account string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	secret, ok := s.secrets[account]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), secret...), nil
}

func (s *MemoryStore) Put(account string, secret []byte) error {
	if account == "" || len(secret) == 0 {
		return errors.New("account and secret are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secrets == nil {
		s.secrets = make(map[string][]byte)
	}
	s.secrets[account] = append([]byte(nil), secret...)
	return nil
}

func (s *MemoryStore) Create(account string, secret []byte) (bool, error) {
	if account == "" || len(secret) == 0 {
		return false, errors.New("account and secret are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secrets == nil {
		s.secrets = make(map[string][]byte)
	}
	if _, exists := s.secrets[account]; exists {
		return false, nil
	}
	s.secrets[account] = append([]byte(nil), secret...)
	return true, nil
}

func (s *MemoryStore) Delete(account string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.secrets[account]; !ok {
		return ErrNotFound
	}
	delete(s.secrets, account)
	return nil
}

func LoadOrCreateIdentity(store Store, account string) (*age.X25519Identity, bool, error) {
	identity, err := LoadIdentity(store, account)
	if err == nil {
		return identity, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	identity, err = age.GenerateX25519Identity()
	if err != nil {
		return nil, false, err
	}
	encoded := []byte(identity.String())
	defer zero(encoded)
	created, err := store.Create(account, encoded)
	if err != nil {
		return nil, false, err
	}
	if created {
		return identity, true, nil
	}
	winner, err := store.Get(account)
	if err != nil {
		return nil, false, err
	}
	defer zero(winner)
	identity, err = age.ParseX25519Identity(string(winner))
	if err != nil {
		return nil, false, fmt.Errorf("parse concurrently stored age identity: %w", err)
	}
	return identity, false, nil
}

// LoadIdentity returns an existing backup identity without creating one.
// Read-only status and doctor paths use this API so observation can never
// mutate Keychain state.
func LoadIdentity(store Store, account string) (*age.X25519Identity, error) {
	secret, err := store.Get(account)
	if err != nil {
		return nil, err
	}
	defer zero(secret)
	identity, err := age.ParseX25519Identity(string(secret))
	if err != nil {
		return nil, fmt.Errorf("parse stored age identity: %w", err)
	}
	return identity, nil
}

func IdentityAccount(canonicalConfigDirectory, box string) string {
	digest := sha256.Sum256([]byte(canonicalConfigDirectory + "\x00" + box))
	return "box:" + box + ":" + hex.EncodeToString(digest[:16])
}

func ExportIdentity(store Store, account, destination string) error {
	secret, err := store.Get(account)
	if err != nil {
		return err
	}
	defer zero(secret)
	identity, err := age.ParseX25519Identity(string(secret))
	if err != nil {
		return fmt.Errorf("parse stored age identity: %w", err)
	}
	if destination == "" {
		return errors.New("destination is required")
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("refusing to replace existing identity %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(destination)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintln(file, identity.String())
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil {
		os.Remove(destination)
		return writeErr
	}
	if syncErr != nil {
		os.Remove(destination)
		return syncErr
	}
	if closeErr != nil {
		os.Remove(destination)
		return closeErr
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr = directory.Sync()
	closeErr = directory.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}

func RecipientFingerprint(recipient age.Recipient) string {
	digest := sha256.Sum256([]byte(fmt.Sprint(recipient)))
	return hex.EncodeToString(digest[:])
}

func zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
