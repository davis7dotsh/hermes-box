package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

const (
	MetadataSchema = 1
	JournalSchema  = 1
)

type Store struct {
	Home string
}

type Names struct {
	VM       string
	DataDisk string
}

type Metadata struct {
	Schema              int       `json:"schema"`
	Name                string    `json:"name"`
	ConfigDir           string    `json:"config_dir"`
	VM                  string    `json:"vm"`
	DataDisk            string    `json:"data_disk"`
	DefinitionSHA256    string    `json:"definition_sha256"`
	VMType              string    `json:"vm_type"`
	Arch                string    `json:"arch"`
	DataDiskSize        int64     `json:"data_disk_size"`
	DataDiskFormat      string    `json:"data_disk_format"`
	DataDiskDir         string    `json:"data_disk_dir,omitempty"`
	DataOwnershipMarker string    `json:"data_ownership_marker"`
	CreatedAt           time.Time `json:"created_at"`
}

type OwnershipBinding struct {
	DefinitionSHA256    string
	DataDiskSize        int64
	DataOwnershipMarker string
}

type JournalArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type RebuildRecovery struct {
	BackupArchive  string            `json:"backup_archive"`
	BackupEnvelope string            `json:"backup_envelope"`
	BackupSHA256   string            `json:"backup_sha256"`
	AppliedLock    string            `json:"applied_lock"`
	Artifacts      []JournalArtifact `json:"artifacts"`
}

type RollbackRecovery struct {
	Component        string `json:"component"`
	PreviousSnapshot string `json:"previous_snapshot"`
	CurrentSnapshot  string `json:"current_snapshot"`
}

type UpdateRecovery struct {
	Component string `json:"component"`
	Snapshot  string `json:"snapshot"`
}

type Journal struct {
	Schema    int               `json:"schema"`
	Operation string            `json:"operation"`
	Phase     string            `json:"phase"`
	Resources []string          `json:"resources,omitempty"`
	Recovery  *RebuildRecovery  `json:"recovery,omitempty"`
	Rollback  *RollbackRecovery `json:"rollback,omitempty"`
	Update    *UpdateRecovery   `json:"update,omitempty"`
	StartedAt time.Time         `json:"started_at"`
}

func NewStore(home string) (*Store, error) {
	if home == "" || !filepath.IsAbs(home) {
		return nil, errors.New("Hermes Box home must be an absolute path")
	}
	home = filepath.Clean(home)
	for _, dir := range []string{
		home,
		filepath.Join(home, "boxes"),
		filepath.Join(home, "locks"),
		filepath.Join(home, "logs"),
		filepath.Join(home, "backups"),
		filepath.Join(home, "artifacts"),
		filepath.Join(home, "lima"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create Hermes Box state directory %s: %w", dir, err)
		}
		info, err := os.Stat(dir)
		if err != nil {
			return nil, fmt.Errorf("inspect Hermes Box state directory %s: %w", dir, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("Hermes Box state directory %s must not be accessible by group or other users", dir)
		}
	}
	return &Store{Home: home}, nil
}

func ResolveHome(environ []string, userHome string) (string, error) {
	selected := ""
	for _, item := range environ {
		key, value, ok := strings.Cut(item, "=")
		if ok && key == "HERMES_BOX_HOME" {
			selected = value
		}
	}
	if selected == "" {
		if userHome == "" || !filepath.IsAbs(userHome) {
			return "", errors.New("user home must be absolute when HERMES_BOX_HOME is unset")
		}
		selected = filepath.Join(userHome, ".hermes-box")
	} else if !filepath.IsAbs(selected) {
		return "", errors.New("HERMES_BOX_HOME must be an absolute path")
	}
	return filepath.Clean(selected), nil
}

func ResourceNames(name string) Names {
	return Names{VM: name, DataDisk: name + "-data"}
}

func NewMetadata(name, configDir string, binding OwnershipBinding, now time.Time) (Metadata, error) {
	if !namePattern.MatchString(name) {
		return Metadata{}, errors.New("invalid box name")
	}
	if now.IsZero() {
		return Metadata{}, errors.New("metadata creation time must not be zero")
	}
	canonicalDir, err := canonicalDirectory(configDir)
	if err != nil {
		return Metadata{}, err
	}
	names := ResourceNames(name)
	return Metadata{
		Schema: MetadataSchema, Name: name, ConfigDir: canonicalDir,
		VM: names.VM, DataDisk: names.DataDisk, DefinitionSHA256: binding.DefinitionSHA256,
		VMType: "vz", Arch: "aarch64", DataDiskSize: binding.DataDiskSize,
		DataDiskFormat: "raw", DataOwnershipMarker: binding.DataOwnershipMarker,
		CreatedAt: now.UTC(),
	}, nil
}

func (s *Store) CreateMetadata(metadata Metadata) error {
	if err := metadata.validate(); err != nil {
		return err
	}
	if err := writeJSONExclusive(s.metadataPath(metadata.Name), metadata, 0o600); err != nil {
		return fmt.Errorf("create metadata for box %q: %w", metadata.Name, err)
	}
	return nil
}

func (s *Store) SaveMetadata(metadata Metadata) error {
	if err := metadata.validate(); err != nil {
		return err
	}
	existing, found, err := s.LoadMetadata(metadata.Name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("cannot update absent metadata for box %q", metadata.Name)
	}
	if existing.ConfigDir != metadata.ConfigDir || existing.VM != metadata.VM || existing.DataDisk != metadata.DataDisk ||
		existing.DefinitionSHA256 != metadata.DefinitionSHA256 || existing.VMType != metadata.VMType || existing.Arch != metadata.Arch ||
		existing.DataDiskSize != metadata.DataDiskSize || existing.DataDiskFormat != metadata.DataDiskFormat ||
		existing.DataDiskDir != metadata.DataDiskDir || existing.DataOwnershipMarker != metadata.DataOwnershipMarker || !existing.CreatedAt.Equal(metadata.CreatedAt) {
		return fmt.Errorf("cannot change ownership identity for box %q", metadata.Name)
	}
	return writeJSONAtomic(s.metadataPath(metadata.Name), metadata, 0o600)
}

func (s *Store) BindDataDisk(name, configDir, directory string) error {
	metadata, err := s.VerifyOwnership(name, configDir)
	if err != nil {
		return err
	}
	if directory == "" || !filepath.IsAbs(directory) {
		return errors.New("data disk directory must be absolute")
	}
	directory = filepath.Clean(directory)
	if metadata.DataDiskDir != "" && metadata.DataDiskDir != directory {
		return fmt.Errorf("data disk for box %q is already bound to %s", name, metadata.DataDiskDir)
	}
	metadata.DataDiskDir = directory
	return writeJSONAtomic(s.metadataPath(name), metadata, 0o600)
}

func (s *Store) UpdateDefinitionSHA256(name, configDir, digest string) error {
	metadata, err := s.VerifyOwnership(name, configDir)
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
		return errors.New("definition SHA-256 is invalid")
	}
	metadata.DefinitionSHA256 = digest
	return writeJSONAtomic(s.metadataPath(name), metadata, 0o600)
}

func (s *Store) LoadMetadata(name string) (Metadata, bool, error) {
	if !namePattern.MatchString(name) {
		return Metadata{}, false, errors.New("invalid box name")
	}
	var metadata Metadata
	found, err := readJSON(s.metadataPath(name), &metadata)
	if err != nil || !found {
		return Metadata{}, found, err
	}
	if err := metadata.validate(); err != nil {
		return Metadata{}, false, fmt.Errorf("invalid metadata for box %q: %w", name, err)
	}
	if metadata.Name != name {
		return Metadata{}, false, fmt.Errorf("metadata name %q does not match requested box %q", metadata.Name, name)
	}
	return metadata, true, nil
}

func (s *Store) RemoveMetadata(name string) error {
	if !namePattern.MatchString(name) {
		return errors.New("invalid box name")
	}
	return removeAndSync(s.metadataPath(name))
}

func (s *Store) VerifyOwnership(name, configDir string) (Metadata, error) {
	metadata, found, err := s.LoadMetadata(name)
	if err != nil {
		return Metadata{}, err
	}
	if !found {
		return Metadata{}, fmt.Errorf("box %q is not owned by this Hermes Box home", name)
	}
	canonicalDir, err := canonicalDirectory(configDir)
	if err != nil {
		return Metadata{}, err
	}
	if metadata.ConfigDir != canonicalDir {
		return Metadata{}, fmt.Errorf(
			"box %q is owned by configuration directory %s, not %s",
			name, metadata.ConfigDir, canonicalDir,
		)
	}
	return metadata, nil
}

func (s *Store) SaveJournal(name string, journal Journal) error {
	if !namePattern.MatchString(name) {
		return errors.New("invalid box name")
	}
	if err := journal.validate(); err != nil {
		return err
	}
	return writeJSONAtomic(s.journalPath(name), journal, 0o600)
}

func (s *Store) LoadJournal(name string) (Journal, bool, error) {
	if !namePattern.MatchString(name) {
		return Journal{}, false, errors.New("invalid box name")
	}
	var journal Journal
	found, err := readJSON(s.journalPath(name), &journal)
	if err != nil || !found {
		return Journal{}, found, err
	}
	if err := journal.validate(); err != nil {
		return Journal{}, false, fmt.Errorf("invalid operation journal for box %q: %w", name, err)
	}
	return journal, true, nil
}

func (s *Store) ClearJournal(name string) error {
	if !namePattern.MatchString(name) {
		return errors.New("invalid box name")
	}
	return removeAndSync(s.journalPath(name))
}

func (s *Store) metadataPath(name string) string {
	return filepath.Join(s.Home, "boxes", name+".json")
}

func (s *Store) journalPath(name string) string {
	return filepath.Join(s.Home, "boxes", name+".journal.json")
}

func (m Metadata) validate() error {
	if m.Schema != MetadataSchema || m.Name == "" || m.ConfigDir == "" || m.VM == "" || m.DataDisk == "" || m.CreatedAt.IsZero() {
		return errors.New("metadata schema, names, owner, and creation time are required")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(m.DefinitionSHA256) {
		return errors.New("metadata definition SHA-256 is invalid")
	}
	if m.VMType != "vz" || m.Arch != "aarch64" || m.DataDiskSize < 1 || m.DataDiskFormat != "raw" {
		return errors.New("metadata VM and disk binding is invalid")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(m.DataOwnershipMarker) {
		return errors.New("metadata data ownership marker is invalid")
	}
	if !namePattern.MatchString(m.Name) {
		return errors.New("metadata name is invalid")
	}
	names := ResourceNames(m.Name)
	if m.VM != names.VM || m.DataDisk != names.DataDisk {
		return fmt.Errorf("metadata resource names must be %q and %q", names.VM, names.DataDisk)
	}
	if !filepath.IsAbs(m.ConfigDir) {
		return errors.New("metadata config_dir must be absolute")
	}
	return nil
}

func (j Journal) validate() error {
	if j.Schema != JournalSchema || j.Operation == "" || j.Phase == "" || j.StartedAt.IsZero() {
		return errors.New("journal schema, operation, phase, and start time are required")
	}
	if j.Operation != "create" && j.Operation != "rebuild" && j.Operation != "rollback" && j.Operation != "update" {
		return fmt.Errorf("unsupported journal operation %q", j.Operation)
	}
	if j.Operation == "create" && j.Phase != "incomplete" {
		return fmt.Errorf("unsupported create journal phase %q", j.Phase)
	}
	if j.Operation == "rebuild" {
		if j.Phase != "prepared" && j.Phase != "root-removed" {
			return fmt.Errorf("unsupported rebuild journal phase %q", j.Phase)
		}
		if j.Recovery == nil || j.Recovery.BackupArchive == "" || j.Recovery.BackupEnvelope == "" ||
			j.Recovery.BackupSHA256 == "" || j.Recovery.AppliedLock == "" || len(j.Recovery.Artifacts) == 0 {
			return errors.New("rebuild journal requires verified backup, applied lock, and artifacts")
		}
		if !filepath.IsAbs(j.Recovery.BackupArchive) || !filepath.IsAbs(j.Recovery.BackupEnvelope) ||
			!filepath.IsAbs(j.Recovery.AppliedLock) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(j.Recovery.BackupSHA256) {
			return errors.New("rebuild journal recovery paths or backup checksum are invalid")
		}
		for _, artifact := range j.Recovery.Artifacts {
			if !filepath.IsAbs(artifact.Path) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(artifact.SHA256) {
				return errors.New("rebuild journal artifact identity is invalid")
			}
		}
	}
	if j.Operation == "rollback" {
		if j.Phase != "prepared" || j.Rollback == nil || j.Rollback.Component == "" {
			return errors.New("rollback journal requires a prepared component recovery")
		}
		if !filepath.IsAbs(j.Rollback.PreviousSnapshot) || !filepath.IsAbs(j.Rollback.CurrentSnapshot) ||
			filepath.Clean(j.Rollback.PreviousSnapshot) == filepath.Clean(j.Rollback.CurrentSnapshot) {
			return errors.New("rollback journal snapshot paths are invalid")
		}
	}
	if j.Operation == "update" {
		if j.Phase != "prepared" || j.Update == nil || j.Update.Component == "" || !filepath.IsAbs(j.Update.Snapshot) {
			return errors.New("update journal requires a prepared component snapshot")
		}
	}
	return nil
}

func canonicalDirectory(path string) (string, error) {
	if path == "" {
		return "", errors.New("configuration directory must not be empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve configuration directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve configuration directory symlinks: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect configuration directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("configuration directory %s is not a directory", resolved)
	}
	return filepath.Clean(resolved), nil
}

func readJSON(path string, target any) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return false, fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return false, fmt.Errorf("decode %s: trailing JSON value", path)
		}
		return false, fmt.Errorf("decode %s: %w", path, err)
	}
	return true, nil
}
