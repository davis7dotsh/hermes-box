package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	ConfigSchema = 1
	LockSchema   = 1
)

var (
	namePattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	sizePattern   = regexp.MustCompile(`^([1-9][0-9]*)(MiB|GiB|TiB)$`)
	hex40Pattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hex64Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	datedImage    = regexp.MustCompile(`/release-[0-9]{8}/`)
)

type Config struct {
	Schema int          `yaml:"schema"`
	Name   string       `yaml:"name"`
	VM     VMConfig     `yaml:"vm"`
	Ports  PortsConfig  `yaml:"ports"`
	Backup BackupConfig `yaml:"backup"`
}

type VMConfig struct {
	CPUs     int    `yaml:"cpus"`
	Memory   string `yaml:"memory"`
	RootDisk string `yaml:"root_disk"`
	DataDisk string `yaml:"data_disk"`
}

type PortsConfig struct {
	Executor int `yaml:"executor"`
}

type BackupConfig struct {
	Keep int `yaml:"keep"`
}

type Lock struct {
	Schema   int          `yaml:"schema"`
	Host     HostLock     `yaml:"host"`
	Ubuntu   UbuntuLock   `yaml:"ubuntu"`
	Tooling  ToolingLock  `yaml:"tooling"`
	Claude   ClaudeLock   `yaml:"claude"`
	Codex    CodexLock    `yaml:"codex"`
	Hermes   HermesLock   `yaml:"hermes"`
	Executor ExecutorLock `yaml:"executor"`
}

type HostLock struct {
	Lima string `yaml:"lima"`
}

type UbuntuLock struct {
	Release           string `yaml:"release"`
	Image             string `yaml:"image"`
	SHA256            string `yaml:"sha256"`
	Provisioner       string `yaml:"provisioner"`
	ProvisionerSHA256 string `yaml:"provisioner_sha256"`
}

type ToolingLock struct {
	Node ToolLock `yaml:"node"`
	UV   ToolLock `yaml:"uv"`
}

type ToolLock struct {
	Version string `yaml:"version"`
	Archive string `yaml:"archive"`
	SHA256  string `yaml:"sha256"`
}

type ClaudeLock struct {
	Version   string `yaml:"version"`
	Package   string `yaml:"package"`
	Tarball   string `yaml:"tarball"`
	Integrity string `yaml:"integrity"`
}

type CodexLock struct {
	Version string `yaml:"version"`
	Archive string `yaml:"archive"`
	SHA256  string `yaml:"sha256"`
}

type HermesLock struct {
	Repository    string `yaml:"repository"`
	Commit        string `yaml:"commit"`
	Archive       string `yaml:"archive"`
	SHA256        string `yaml:"sha256"`
	UVLockSHA256  string `yaml:"uv_lock_sha256"`
	PythonArchive string `yaml:"python_archive"`
	PythonSHA256  string `yaml:"python_sha256"`
	WheelsArchive string `yaml:"wheels_archive"`
	WheelsSHA256  string `yaml:"wheels_sha256"`
}

type ExecutorLock struct {
	Image            string `yaml:"image"`
	LinuxARM64Digest string `yaml:"linux_arm64_digest"`
}

type Bundle struct {
	Config     Config
	Lock       Lock
	ConfigPath string
	LockPath   string
	Dir        string
}

func Load(explicitPath, cwd string, environ []string) (Bundle, error) {
	if err := ValidateEnvironment(environ); err != nil {
		return Bundle{}, err
	}
	configPath, err := ResolvePath(explicitPath, cwd, environ)
	if err != nil {
		return Bundle{}, err
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return Bundle{}, err
	}
	dir := filepath.Dir(configPath)
	lockPath := filepath.Join(dir, "hermes-box.lock")
	lock, err := LoadLock(lockPath)
	if err != nil {
		return Bundle{}, err
	}
	return Bundle{
		Config: cfg, Lock: lock, ConfigPath: configPath, LockPath: lockPath, Dir: dir,
	}, nil
}

func ResolvePath(explicitPath, cwd string, environ []string) (string, error) {
	if cwd == "" {
		return "", errors.New("working directory must not be empty")
	}
	selected := explicitPath
	if selected == "" {
		selected = environment(environ)["HERMES_BOX_CONFIG"]
	}
	if selected == "" {
		selected = filepath.Join(cwd, "hermes-box.yaml")
	} else if !filepath.IsAbs(selected) {
		selected = filepath.Join(cwd, selected)
	}
	abs, err := filepath.Abs(selected)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func ValidateEnvironment(environ []string) error {
	allowed := map[string]struct{}{
		"HERMES_BOX_CONFIG": {},
		"HERMES_BOX_HOME":   {},
	}
	for key := range environment(environ) {
		if strings.HasPrefix(key, "HERMES_BOX_") {
			if _, ok := allowed[key]; !ok {
				return fmt.Errorf("unknown Hermes Box environment setting %q", key)
			}
		}
	}
	return nil
}

func LoadConfig(path string) (Config, error) {
	var cfg Config
	if err := decodeStrictFile(path, &cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config %s: %w", path, err)
	}
	return cfg, nil
}

func LoadLock(path string) (Lock, error) {
	var lock Lock
	if err := decodeStrictFile(path, &lock); err != nil {
		return Lock{}, err
	}
	if err := lock.Validate(); err != nil {
		return Lock{}, fmt.Errorf("validate lock %s: %w", path, err)
	}
	return lock, nil
}

func (c Config) Validate() error {
	if c.Schema != ConfigSchema {
		return fmt.Errorf("schema must be exactly %d", ConfigSchema)
	}
	if !namePattern.MatchString(c.Name) {
		return errors.New("name must match [a-z][a-z0-9-]{0,31}")
	}
	if c.VM.CPUs < 1 || c.VM.CPUs > 64 {
		return errors.New("vm.cpus must be between 1 and 64")
	}
	if err := validateSize("vm.memory", c.VM.Memory, 512<<20, 512<<30); err != nil {
		return err
	}
	if err := validateSize("vm.root_disk", c.VM.RootDisk, 10<<30, 16<<40); err != nil {
		return err
	}
	if err := validateSize("vm.data_disk", c.VM.DataDisk, 1<<30, 16<<40); err != nil {
		return err
	}
	if c.Ports.Executor < 1024 || c.Ports.Executor > 65535 {
		return errors.New("ports.executor must be between 1024 and 65535")
	}
	if c.Backup.Keep < 1 || c.Backup.Keep > 100 {
		return errors.New("backup.keep must be between 1 and 100")
	}
	return nil
}

func (l Lock) Validate() error {
	if l.Schema != LockSchema {
		return fmt.Errorf("schema must be exactly %d", LockSchema)
	}
	if l.Host.Lima != "2.1.3" {
		return errors.New("host.lima must be the qualified version 2.1.3")
	}
	if l.Ubuntu.Release != "26.04" {
		return errors.New("ubuntu.release must be 26.04")
	}
	for _, item := range []struct{ name, value string }{
		{"ubuntu.image", l.Ubuntu.Image}, {"ubuntu.provisioner", l.Ubuntu.Provisioner},
		{"tooling.node.archive", l.Tooling.Node.Archive}, {"tooling.uv.archive", l.Tooling.UV.Archive},
		{"claude.tarball", l.Claude.Tarball}, {"codex.archive", l.Codex.Archive},
		{"hermes.repository", l.Hermes.Repository}, {"hermes.archive", l.Hermes.Archive},
		{"hermes.python_archive", l.Hermes.PythonArchive}, {"hermes.wheels_archive", l.Hermes.WheelsArchive},
	} {
		if err := validateHTTPSURL(item.name, item.value); err != nil {
			return err
		}
	}
	if !datedImage.MatchString(l.Ubuntu.Image) {
		return errors.New("ubuntu.image must use a dated release directory")
	}
	for _, item := range []struct{ name, value string }{
		{"ubuntu.sha256", l.Ubuntu.SHA256}, {"ubuntu.provisioner_sha256", l.Ubuntu.ProvisionerSHA256},
		{"tooling.node.sha256", l.Tooling.Node.SHA256}, {"tooling.uv.sha256", l.Tooling.UV.SHA256},
		{"codex.sha256", l.Codex.SHA256}, {"hermes.sha256", l.Hermes.SHA256},
		{"hermes.uv_lock_sha256", l.Hermes.UVLockSHA256}, {"hermes.python_sha256", l.Hermes.PythonSHA256},
		{"hermes.wheels_sha256", l.Hermes.WheelsSHA256},
	} {
		if !hex64Pattern.MatchString(item.value) {
			return fmt.Errorf("%s must be 64 lowercase hexadecimal characters", item.name)
		}
	}
	if l.Tooling.Node.Version == "" || l.Tooling.UV.Version == "" || l.Claude.Version == "" || l.Codex.Version == "" {
		return errors.New("component versions must not be empty")
	}
	if l.Claude.Package == "" {
		return errors.New("claude.package must not be empty")
	}
	integrity, ok := strings.CutPrefix(l.Claude.Integrity, "sha512-")
	decodedIntegrity, err := base64.StdEncoding.Strict().DecodeString(integrity)
	if !ok || err != nil || len(decodedIntegrity) != 64 {
		return errors.New("claude.integrity must be a sha512 SRI value")
	}
	if !hex40Pattern.MatchString(l.Hermes.Commit) {
		return errors.New("hermes.commit must be a full lowercase hexadecimal Git commit")
	}
	if err := validatePinnedImage(l.Executor.Image); err != nil {
		return fmt.Errorf("executor.image %w", err)
	}
	if !digestPattern.MatchString(l.Executor.LinuxARM64Digest) {
		return errors.New("executor.linux_arm64_digest must be a full sha256 digest")
	}
	return nil
}

func decodeStrictFile(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode %s: multiple YAML documents are not allowed", path)
		}
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func validateSize(name, value string, minimum, maximum int64) error {
	match := sizePattern.FindStringSubmatch(value)
	if match == nil {
		return fmt.Errorf("%s must be a positive MiB, GiB, or TiB value", name)
	}
	amount, _ := strconv.ParseInt(match[1], 10, 64)
	multiplier := int64(1 << 20)
	if match[2] == "GiB" {
		multiplier = 1 << 30
	} else if match[2] == "TiB" {
		multiplier = 1 << 40
	}
	if amount > maximum/multiplier {
		return fmt.Errorf("%s exceeds the supported maximum", name)
	}
	bytes := amount * multiplier
	if bytes < minimum || bytes > maximum {
		return fmt.Errorf("%s is outside the supported range", name)
	}
	return nil
}

func validateHTTPSURL(name, value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("%s must be an absolute HTTPS URL", name)
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("%s must not contain a fragment", name)
	}
	return nil
}

func validatePinnedImage(value string) error {
	prefix, digest, ok := strings.Cut(value, "@sha256:")
	if !ok || prefix == "" || !hex64Pattern.MatchString(digest) || strings.ContainsAny(prefix, " \t\r\n") {
		return errors.New("must be an image tag pinned by a full sha256 digest")
	}
	lastSlash := strings.LastIndexByte(prefix, '/')
	if !strings.Contains(prefix[lastSlash+1:], ":") {
		return errors.New("must include an explicit version tag before its digest")
	}
	return nil
}

func environment(environ []string) map[string]string {
	result := make(map[string]string, len(environ))
	for _, item := range environ {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}

func PortAvailable(port int) bool {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}
