package identity

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const filename = "identity.json"

type Identity struct {
	NodeID     int    `json:"nodeId"`
	NodeName   string `json:"nodeName"`
	Credential string `json:"credential"`
}

type Store struct {
	directory string
}

func NewStore(directory string) *Store {
	return &Store{directory: directory}
}

func (s *Store) Path() string {
	return filepath.Join(s.directory, filename)
}

func (s *Store) Load() (Identity, error) {
	info, err := os.Stat(s.Path())
	if err != nil {
		return Identity{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Identity{}, fmt.Errorf("identity file %s must not be accessible by group or others", s.Path())
	}
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		return Identity{}, err
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return Identity{}, fmt.Errorf("decode identity: %w", err)
	}
	if id.NodeID <= 0 || id.Credential == "" {
		return Identity{}, errors.New("identity is incomplete")
	}
	return id, nil
}

func (s *Store) Save(id Identity) error {
	if id.NodeID <= 0 || id.Credential == "" {
		return errors.New("identity is incomplete")
	}
	if err := os.MkdirAll(s.directory, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(s.directory, 0o700); err != nil {
		return fmt.Errorf("secure state directory: %w", err)
	}
	raw, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return fmt.Errorf("encode identity: %w", err)
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(s.directory, ".identity-*")
	if err != nil {
		return fmt.Errorf("create identity temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("secure identity temp file: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write identity: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync identity: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close identity: %w", err)
	}
	if err := os.Rename(tmpName, s.Path()); err != nil {
		return fmt.Errorf("replace identity: %w", err)
	}
	dir, err := os.Open(s.directory)
	if err != nil {
		return fmt.Errorf("open state directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}
