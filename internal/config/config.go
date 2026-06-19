package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultMachineName   = "hermes-box"
	defaultBuilderName   = "hermes-builder"
	defaultSSHPort       = 2222
	defaultCPUs          = 4
	defaultMemoryMiB     = 8192
	defaultStorageGB     = 15
	defaultOverlayGB     = 6
	defaultNetworkMode   = "none"
	defaultHermesCommit  = "81eaedd0f5c471c7ee748990066135a684f3c962"
	defaultExecutorPort  = 4788
	defaultExecutorImage = "ghcr.io/rhyssullivan/executor-selfhost:v1.5.12@sha256:e40b2179c005b3124e794e9a8505341db46d0a9a1631e7f3fdcd023462ecf70b"
)

type Config struct {
	MachineName     string
	BuilderName     string
	SSHPort         int
	CPUs            int
	MemoryMiB       int
	StorageGB       int
	OverlayGB       int
	NetworkMode     string
	HermesCommit    string
	ExecutorEnabled bool
	ExecutorPort    int
	ExecutorImage   string
	DataDir         string
	SSHKey          string
	ConfigFile      string
}

var keys = map[string]func(*Config, string) error{
	"HERMES_BOX_MACHINE_NAME": func(c *Config, value string) error {
		c.MachineName = value
		return nil
	},
	"HERMES_BOX_BUILDER_NAME": func(c *Config, value string) error {
		c.BuilderName = value
		return nil
	},
	"HERMES_BOX_SSH_PORT": func(c *Config, value string) error {
		return setInt(&c.SSHPort, "HERMES_BOX_SSH_PORT", value)
	},
	"HERMES_BOX_CPUS": func(c *Config, value string) error {
		return setInt(&c.CPUs, "HERMES_BOX_CPUS", value)
	},
	"HERMES_BOX_MEMORY_MIB": func(c *Config, value string) error {
		return setInt(&c.MemoryMiB, "HERMES_BOX_MEMORY_MIB", value)
	},
	"HERMES_BOX_STORAGE_GB": func(c *Config, value string) error {
		return setInt(&c.StorageGB, "HERMES_BOX_STORAGE_GB", value)
	},
	"HERMES_BOX_OVERLAY_GB": func(c *Config, value string) error {
		return setInt(&c.OverlayGB, "HERMES_BOX_OVERLAY_GB", value)
	},
	"HERMES_BOX_NETWORK_MODE": func(c *Config, value string) error {
		c.NetworkMode = value
		return nil
	},
	"HERMES_BOX_HERMES_COMMIT": func(c *Config, value string) error {
		c.HermesCommit = value
		return nil
	},
	"HERMES_BOX_EXECUTOR_ENABLED": func(c *Config, value string) error {
		return setBool(&c.ExecutorEnabled, "HERMES_BOX_EXECUTOR_ENABLED", value)
	},
	"HERMES_BOX_EXECUTOR_PORT": func(c *Config, value string) error {
		return setInt(&c.ExecutorPort, "HERMES_BOX_EXECUTOR_PORT", value)
	},
	"HERMES_BOX_EXECUTOR_IMAGE": func(c *Config, value string) error {
		c.ExecutorImage = value
		return nil
	},
	"HERMES_BOX_DATA_DIR": func(c *Config, value string) error {
		c.DataDir = value
		return nil
	},
	"HERMES_BOX_SSH_KEY": func(c *Config, value string) error {
		c.SSHKey = value
		return nil
	},
}

func Load(projectRoot string, environ []string) (Config, error) {
	cfg := Config{
		MachineName:   defaultMachineName,
		BuilderName:   defaultBuilderName,
		SSHPort:       defaultSSHPort,
		CPUs:          defaultCPUs,
		MemoryMiB:     defaultMemoryMiB,
		StorageGB:     defaultStorageGB,
		OverlayGB:     defaultOverlayGB,
		NetworkMode:   defaultNetworkMode,
		HermesCommit:  defaultHermesCommit,
		ExecutorPort:  defaultExecutorPort,
		ExecutorImage: defaultExecutorImage,
	}

	env := environment(environ)
	cfg.ConfigFile = filepath.Join(projectRoot, "hermes-box.conf")
	if value, ok := env["HERMES_BOX_CONFIG"]; ok && value != "" {
		cfg.ConfigFile = value
	}

	if err := loadFile(&cfg, cfg.ConfigFile); err != nil {
		return Config{}, err
	}
	for key, setter := range keys {
		if value, ok := env[key]; ok {
			if err := setter(&cfg, value); err != nil {
				return Config{}, err
			}
		}
	}
	cfg.applyEmptyDefaults()
	if cfg.DataDir != "" {
		if !filepath.IsAbs(cfg.DataDir) {
			cfg.DataDir = filepath.Join(projectRoot, cfg.DataDir)
		}
		cfg.DataDir = filepath.Clean(cfg.DataDir)
	}
	if cfg.SSHKey != "" {
		if !filepath.IsAbs(cfg.SSHKey) {
			cfg.SSHKey = filepath.Join(projectRoot, cfg.SSHKey)
		}
		cfg.SSHKey = filepath.Clean(cfg.SSHKey)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) applyEmptyDefaults() {
	if c.MachineName == "" {
		c.MachineName = defaultMachineName
	}
	if c.BuilderName == "" {
		c.BuilderName = defaultBuilderName
	}
	if c.NetworkMode == "" {
		c.NetworkMode = defaultNetworkMode
	}
	if c.HermesCommit == "" {
		c.HermesCommit = defaultHermesCommit
	}
	if c.ExecutorImage == "" {
		c.ExecutorImage = defaultExecutorImage
	}
}

func (c Config) Validate() error {
	if c.MachineName == "" {
		return fmt.Errorf("HERMES_BOX_MACHINE_NAME must not be empty")
	}
	if c.BuilderName == "" {
		return fmt.Errorf("HERMES_BOX_BUILDER_NAME must not be empty")
	}
	if c.MachineName == c.BuilderName {
		return fmt.Errorf("runtime and builder machine names must differ")
	}
	if c.SSHPort < 1 || c.SSHPort > 65535 {
		return fmt.Errorf("HERMES_BOX_SSH_PORT must be between 1 and 65535")
	}
	if c.ExecutorEnabled && (c.ExecutorPort < 1 || c.ExecutorPort > 65535) {
		return fmt.Errorf("HERMES_BOX_EXECUTOR_PORT must be between 1 and 65535")
	}
	if c.ExecutorEnabled && c.ExecutorPort == c.SSHPort {
		return fmt.Errorf("HERMES_BOX_EXECUTOR_PORT must differ from HERMES_BOX_SSH_PORT")
	}
	if c.ExecutorEnabled {
		if err := validatePinnedImage(c.ExecutorImage); err != nil {
			return fmt.Errorf("HERMES_BOX_EXECUTOR_IMAGE %w", err)
		}
	}
	for name, value := range map[string]int{
		"HERMES_BOX_CPUS":       c.CPUs,
		"HERMES_BOX_MEMORY_MIB": c.MemoryMiB,
		"HERMES_BOX_STORAGE_GB": c.StorageGB,
		"HERMES_BOX_OVERLAY_GB": c.OverlayGB,
	} {
		if value < 1 {
			return fmt.Errorf("%s must be a positive integer", name)
		}
	}
	switch c.NetworkMode {
	case "full", "none", "strict":
	default:
		return fmt.Errorf("HERMES_BOX_NETWORK_MODE must be strict, full, or none")
	}
	if c.HermesCommit != "" {
		if len(c.HermesCommit) != 40 {
			return fmt.Errorf("HERMES_BOX_HERMES_COMMIT must be a full 40-character Git commit")
		}
		if _, err := strconv.ParseUint(c.HermesCommit[:16], 16, 64); err != nil {
			return fmt.Errorf("HERMES_BOX_HERMES_COMMIT must be hexadecimal")
		}
		if _, err := strconv.ParseUint(c.HermesCommit[16:32], 16, 64); err != nil {
			return fmt.Errorf("HERMES_BOX_HERMES_COMMIT must be hexadecimal")
		}
		if _, err := strconv.ParseUint(c.HermesCommit[32:], 16, 64); err != nil {
			return fmt.Errorf("HERMES_BOX_HERMES_COMMIT must be hexadecimal")
		}
	}
	return nil
}

func loadFile(cfg *Config, path string) error {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open config %s: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		key, value, ok, err := parseAssignment(scanner.Text())
		if err != nil {
			return fmt.Errorf("%s:%d: %w", path, lineNumber, err)
		}
		if !ok {
			continue
		}
		setter, known := keys[key]
		if !known {
			continue
		}
		if err := setter(cfg, value); err != nil {
			return fmt.Errorf("%s:%d: %w", path, lineNumber, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	return nil
}

func parseAssignment(line string) (string, string, bool, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	equal := strings.IndexByte(line, '=')
	if equal < 1 {
		return "", "", false, fmt.Errorf("expected KEY=VALUE assignment")
	}
	key := strings.TrimSpace(line[:equal])
	if !validIdentifier(key) {
		return "", "", false, fmt.Errorf("invalid variable name %q", key)
	}
	value, err := parseValue(strings.TrimSpace(line[equal+1:]))
	if err != nil {
		return "", "", false, err
	}
	return key, value, true, nil
}

func parseValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	switch value[0] {
	case '\'':
		end := strings.IndexByte(value[1:], '\'')
		if end < 0 {
			return "", fmt.Errorf("unterminated single-quoted value")
		}
		end++
		if strings.TrimSpace(value[end+1:]) != "" && !strings.HasPrefix(strings.TrimSpace(value[end+1:]), "#") {
			return "", fmt.Errorf("unexpected content after quoted value")
		}
		return value[1:end], nil
	case '"':
		end := quotedEnd(value)
		if end < 0 {
			return "", fmt.Errorf("unterminated double-quoted value")
		}
		if strings.TrimSpace(value[end+1:]) != "" && !strings.HasPrefix(strings.TrimSpace(value[end+1:]), "#") {
			return "", fmt.Errorf("unexpected content after quoted value")
		}
		parsed, err := strconv.Unquote(value[:end+1])
		if err != nil {
			return "", fmt.Errorf("invalid double-quoted value: %w", err)
		}
		return parsed, nil
	default:
		if comment := strings.Index(value, " #"); comment >= 0 {
			value = value[:comment]
		}
		return strings.TrimSpace(value), nil
	}
}

func quotedEnd(value string) int {
	escaped := false
	for index := 1; index < len(value); index++ {
		if escaped {
			escaped = false
			continue
		}
		if value[index] == '\\' {
			escaped = true
			continue
		}
		if value[index] == '"' {
			return index
		}
	}
	return -1
}

func validIdentifier(value string) bool {
	if value == "" || !isIdentifierStart(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !isIdentifierStart(value[index]) && (value[index] < '0' || value[index] > '9') {
			return false
		}
	}
	return true
}

func isIdentifierStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func setInt(target *int, name, value string) error {
	if value == "" {
		switch name {
		case "HERMES_BOX_SSH_PORT":
			*target = defaultSSHPort
		case "HERMES_BOX_CPUS":
			*target = defaultCPUs
		case "HERMES_BOX_MEMORY_MIB":
			*target = defaultMemoryMiB
		case "HERMES_BOX_STORAGE_GB":
			*target = defaultStorageGB
		case "HERMES_BOX_OVERLAY_GB":
			*target = defaultOverlayGB
		case "HERMES_BOX_EXECUTOR_PORT":
			*target = defaultExecutorPort
		}
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("%s must be an integer", name)
	}
	*target = parsed
	return nil
}

func setBool(target *bool, name, value string) error {
	if value == "" {
		*target = false
		return nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fmt.Errorf("%s must be true or false", name)
	}
	*target = parsed
	return nil
}

func validatePinnedImage(value string) error {
	prefix, digest, ok := strings.Cut(value, "@sha256:")
	if !ok || prefix == "" || len(digest) != 64 || strings.ContainsAny(prefix, " \t\r\n") {
		return fmt.Errorf("must be an image tag pinned by a full sha256 digest")
	}
	lastSlash := strings.LastIndexByte(prefix, '/')
	if !strings.Contains(prefix[lastSlash+1:], ":") {
		return fmt.Errorf("must include an explicit version tag before its digest")
	}
	for start := 0; start < len(digest); start += 16 {
		if _, err := strconv.ParseUint(digest[start:start+16], 16, 64); err != nil {
			return fmt.Errorf("digest must be hexadecimal")
		}
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
