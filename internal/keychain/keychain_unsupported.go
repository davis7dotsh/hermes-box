//go:build !darwin || !cgo

package keychain

import "fmt"

type KeychainStore struct{}

func New(service string) (*KeychainStore, error) {
	return nil, fmt.Errorf("%w: requires darwin with cgo enabled", ErrUnavailable)
}

func (s *KeychainStore) Get(string) ([]byte, error) { return nil, ErrUnavailable }
func (s *KeychainStore) Put(string, []byte) error   { return ErrUnavailable }
func (s *KeychainStore) Create(string, []byte) (bool, error) {
	return false, ErrUnavailable
}
func (s *KeychainStore) Delete(string) error { return ErrUnavailable }
