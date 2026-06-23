package lima

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/davis7dotsh/hermes-box/internal/config"
	"gopkg.in/yaml.v3"
)

const (
	QualifiedVersion  = "2.1.3"
	ExecutorGuestPort = 4788
	UbuntuImageURL    = "https://cloud-images.ubuntu.com/releases/26.04/release-20260612/ubuntu-26.04-server-cloudimg-arm64.img"
	UbuntuImageSHA256 = "5e1c212ac29354dbf51c5b1926d8a359de57ca8c2d2bdacf17651129c29791cb"
)

var versionPattern = regexp.MustCompile(`(?:^|\s)v?([0-9]+\.[0-9]+\.[0-9]+)(?:\s|$)`)
var resourceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

type Invocation struct {
	Args  []string
	Env   []string
	Stdin io.Reader
}

type Result struct {
	Stdout []byte
	Stderr []byte
}

type Runner interface {
	Run(context.Context, Invocation) (Result, error)
}

type OSRunner struct {
	Binary string
}

type Client struct {
	home   string
	runner Runner
}

type Instance struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Arch   string `json:"arch"`
	VMType string `json:"vmType"`
}

type Disk struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Size    int64  `json:"size"`
	Format  string `json:"format"`
	Dir     string `json:"dir"`
	InUseBy string `json:"instance"`
}

func (r OSRunner) Run(ctx context.Context, invocation Invocation) (Result, error) {
	binary := r.Binary
	if binary == "" {
		binary = "limactl"
	}
	command := exec.CommandContext(ctx, binary, invocation.Args...)
	command.Env = mergeEnvironment(os.Environ(), invocation.Env)
	command.Stdin = invocation.Stdin
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, err
}

func New(home string, runner Runner) (*Client, error) {
	if home == "" || !filepath.IsAbs(home) {
		return nil, errors.New("LIMA_HOME must be an absolute path")
	}
	if runner == nil {
		return nil, errors.New("Lima runner must not be nil")
	}
	home = filepath.Clean(home)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create LIMA_HOME: %w", err)
	}
	info, err := os.Stat(home)
	if err != nil {
		return nil, fmt.Errorf("inspect LIMA_HOME: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("LIMA_HOME must not be accessible by group or other users")
	}
	return &Client{home: home, runner: runner}, nil
}

func (c *Client) Home() string {
	return c.home
}

func (c *Client) DefinitionPath(name string) (string, error) {
	if err := validateResourceName(name); err != nil {
		return "", err
	}
	return filepath.Join(c.home, "_hermes-box-definitions", name+".yaml"), nil
}

func (c *Client) Version(ctx context.Context) (string, error) {
	version, err := c.DetectVersion(ctx)
	if err != nil {
		return "", err
	}
	if version != QualifiedVersion {
		return "", fmt.Errorf("Lima %s is unsupported; install %s", version, QualifiedVersion)
	}
	return version, nil
}

// DetectVersion reports the installed Lima version without applying the
// runtime qualification policy. Diagnostic commands use it so a fresh host
// can describe an absent or outdated installation; mutating preflight uses
// Version and still fails closed on anything except QualifiedVersion.
func (c *Client) DetectVersion(ctx context.Context) (string, error) {
	result, err := c.run(ctx, []string{"--version"}, nil)
	if err != nil {
		return "", err
	}
	match := versionPattern.FindSubmatch(result.Stdout)
	if match == nil {
		return "", fmt.Errorf("parse Lima version from %q", strings.TrimSpace(string(result.Stdout)))
	}
	version := string(match[1])
	return version, nil
}

func (c *Client) Inspect(ctx context.Context) ([]Instance, error) {
	result, err := c.run(ctx, []string{"list", "--format", "json"}, nil)
	if err != nil {
		return nil, err
	}
	return decodeJSONL[Instance](result.Stdout, "Lima instance list")
}

func (c *Client) InspectInstance(ctx context.Context, name string) (Instance, bool, error) {
	if err := validateResourceName(name); err != nil {
		return Instance{}, false, err
	}
	result, err := c.run(ctx, []string{"list", "--format", "json", name}, nil)
	if err != nil {
		return Instance{}, false, err
	}
	instances, err := decodeJSONL[Instance](result.Stdout, "Lima instance list")
	if err != nil {
		return Instance{}, false, err
	}
	if len(instances) == 0 {
		return Instance{}, false, nil
	}
	if len(instances) != 1 || instances[0].Name != name {
		return Instance{}, false, fmt.Errorf("Lima returned unexpected instances while inspecting %q", name)
	}
	return instances[0], true, nil
}

func (c *Client) InspectDisks(ctx context.Context) ([]Disk, error) {
	result, err := c.run(ctx, []string{"disk", "list", "--json"}, nil)
	if err != nil {
		return nil, err
	}
	return decodeJSONL[Disk](result.Stdout, "Lima disk list")
}

func (c *Client) InspectDisk(ctx context.Context, name string) (Disk, bool, error) {
	if err := validateResourceName(name); err != nil {
		return Disk{}, false, err
	}
	result, err := c.run(ctx, []string{"disk", "list", "--json", name}, nil)
	if err != nil {
		return Disk{}, false, err
	}
	disks, err := decodeJSONL[Disk](result.Stdout, "Lima disk list")
	if err != nil {
		return Disk{}, false, err
	}
	if len(disks) == 0 {
		return Disk{}, false, nil
	}
	if len(disks) != 1 || disks[0].Name != name {
		return Disk{}, false, fmt.Errorf("Lima returned unexpected disks while inspecting %q", name)
	}
	return disks[0], true, nil
}

func (c *Client) CreateDisk(ctx context.Context, name, size string) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	if size == "" {
		return errors.New("disk size must not be empty")
	}
	// VZ presents additional disks as raw devices. Create the persistent disk
	// in that final representation so its recorded ownership identity does not
	// change when Lima attaches it to a VZ instance.
	_, err := c.run(ctx, []string{"disk", "create", name, "--size", size, "--format", "raw", "--tty=false"}, nil)
	return err
}

func (c *Client) DeleteDisk(ctx context.Context, name string) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	_, err := c.run(ctx, []string{"disk", "delete", name, "--tty=false"}, nil)
	return err
}

func (c *Client) Create(ctx context.Context, name string, definition []byte) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	if len(definition) == 0 {
		return errors.New("Lima definition must not be empty")
	}
	path, err := c.SaveDefinition(name, definition)
	if err != nil {
		return err
	}
	_, err = c.run(ctx, []string{"create", "--name", name, "--tty=false", path}, nil)
	return err
}

func (c *Client) SaveDefinition(name string, definition []byte) (string, error) {
	if err := validateResourceName(name); err != nil {
		return "", err
	}
	if len(definition) == 0 {
		return "", errors.New("Lima definition must not be empty")
	}
	definitions := filepath.Join(c.home, "_hermes-box-definitions")
	if err := os.MkdirAll(definitions, 0o700); err != nil {
		return "", fmt.Errorf("create Lima definition directory: %w", err)
	}
	path := filepath.Join(definitions, name+".yaml")
	if err := writeAtomic(path, definition); err != nil {
		return "", err
	}
	return path, nil
}

func (c *Client) Start(ctx context.Context, name string) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	_, err := c.run(ctx, []string{"start", name, "--tty=false"}, nil)
	return err
}

func (c *Client) Stop(ctx context.Context, name string, force bool) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	args := []string{"stop", name, "--tty=false"}
	if force {
		args = append(args, "--force")
	}
	_, err := c.run(ctx, args, nil)
	return err
}

func (c *Client) Delete(ctx context.Context, name string) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	_, err := c.run(ctx, []string{"delete", name, "--tty=false"}, nil)
	return err
}

func (c *Client) Shell(ctx context.Context, instance, workdir string, args ...string) (Result, error) {
	if err := validateResourceName(instance); err != nil {
		return Result{}, err
	}
	if workdir == "" || !strings.HasPrefix(workdir, "/") {
		return Result{}, errors.New("guest work directory must be absolute")
	}
	commandArgs := []string{"shell", "--workdir", workdir, instance, "--"}
	commandArgs = append(commandArgs, args...)
	return c.run(ctx, commandArgs, nil)
}

func (c *Client) Copy(ctx context.Context, recursive bool, sources []string, target string) error {
	if len(sources) == 0 || target == "" {
		return errors.New("copy requires at least one source and a target")
	}
	args := []string{"copy", "--backend=scp"}
	if recursive {
		args = append(args, "--recursive")
	}
	args = append(args, "--")
	args = append(args, sources...)
	args = append(args, target)
	_, err := c.run(ctx, args, nil)
	return err
}

func GenerateYAML(cfg config.Config, lock config.Lock) ([]byte, error) {
	return generateYAML(cfg, lock, lock.Ubuntu.Image)
}

func GenerateYAMLWithImage(cfg config.Config, lock config.Lock, localPath string) ([]byte, error) {
	if localPath == "" || !filepath.IsAbs(localPath) {
		return nil, errors.New("local Ubuntu image path must be absolute")
	}
	localPath = filepath.Clean(localPath)
	info, err := os.Lstat(localPath)
	if err != nil {
		return nil, fmt.Errorf("inspect local Ubuntu image: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("local Ubuntu image must be a regular file, not a symlink or directory")
	}
	location := (&url.URL{Scheme: "file", Path: localPath}).String()
	return generateYAML(cfg, lock, location)
}

func generateYAML(cfg config.Config, lock config.Lock, imageLocation string) ([]byte, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if err := lock.Validate(); err != nil {
		return nil, fmt.Errorf("invalid lock: %w", err)
	}
	definition := vmDefinition{
		MinimumLimaVersion: QualifiedVersion,
		VMType:             "vz",
		Arch:               "aarch64",
		Images: []imageDefinition{{
			Location: imageLocation,
			Arch:     "aarch64",
			Digest:   "sha256:" + lock.Ubuntu.SHA256,
		}},
		CPUs:   cfg.VM.CPUs,
		Memory: cfg.VM.Memory,
		Disk:   cfg.VM.RootDisk,
		Mounts: []mountDefinition{},
		Containerd: containerdDefinition{
			System: false,
			User:   false,
		},
		UpgradePackages:   false,
		PropagateProxyEnv: false,
		Video:             videoDefinition{Display: "none"},
		SSH: sshDefinition{
			LoadDotSSHPubKeys: false,
			ForwardAgent:      false,
			ForwardX11:        false,
			ForwardX11Trusted: false,
		},
		User: userDefinition{
			Name: "agent", Home: "/home/agent", Shell: "/bin/bash", UID: 1000,
		},
		AdditionalDisks: []additionalDiskDefinition{{
			Name: cfg.Name + "-data", Format: true, FSType: "ext4",
		}},
		PortForwards: []portForwardDefinition{
			{GuestPort: ExecutorGuestPort, HostPort: cfg.Ports.Executor, HostIP: "127.0.0.1"},
			{GuestIP: "0.0.0.0", Proto: "any", Ignore: true},
		},
	}
	data, err := yaml.Marshal(definition)
	if err != nil {
		return nil, fmt.Errorf("encode Lima definition: %w", err)
	}
	return data, nil
}

func (c *Client) run(ctx context.Context, args []string, stdin io.Reader) (Result, error) {
	result, err := c.runner.Run(ctx, Invocation{
		Args: args, Env: []string{"LIMA_HOME=" + c.home}, Stdin: stdin,
	})
	if err != nil {
		message := strings.TrimSpace(string(result.Stderr))
		if message == "" {
			message = strings.TrimSpace(string(result.Stdout))
		}
		if message == "" {
			return result, fmt.Errorf("limactl %s: %w", strings.Join(args, " "), err)
		}
		return result, fmt.Errorf("limactl %s: %s: %w", strings.Join(args, " "), message, err)
	}
	return result, nil
}

func decodeJSONL[T any](data []byte, label string) ([]T, error) {
	var values []T
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		trimmed := bytes.TrimSpace(scanner.Bytes())
		if len(trimmed) == 0 {
			continue
		}
		if trimmed[0] == '[' {
			return nil, fmt.Errorf("decode %s line %d: expected one JSON object per line", label, line)
		}
		var value T
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode %s line %d: %w", label, line, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("decode %s line %d: trailing JSON value", label, line)
		}
		values = append(values, value)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	return values, nil
}

func validateResourceName(name string) error {
	if !resourceNamePattern.MatchString(name) {
		return fmt.Errorf("invalid Lima resource name %q", name)
	}
	return nil
}

func mergeEnvironment(base, overrides []string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	order := make([]string, 0, len(base)+len(overrides))
	for _, item := range append(append([]string{}, base...), overrides...) {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	result := make([]string, 0, len(order))
	for _, key := range order {
		result = append(result, key+"="+values[key])
	}
	return result
}

func writeAtomic(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".definition-*")
	if err != nil {
		return fmt.Errorf("create temporary Lima definition: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install Lima definition: %w", err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open Lima definition directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync Lima definition directory: %w", err)
	}
	return nil
}

type vmDefinition struct {
	MinimumLimaVersion string                     `yaml:"minimumLimaVersion"`
	VMType             string                     `yaml:"vmType"`
	Arch               string                     `yaml:"arch"`
	Images             []imageDefinition          `yaml:"images"`
	CPUs               int                        `yaml:"cpus"`
	Memory             string                     `yaml:"memory"`
	Disk               string                     `yaml:"disk"`
	Mounts             []mountDefinition          `yaml:"mounts"`
	Containerd         containerdDefinition       `yaml:"containerd"`
	UpgradePackages    bool                       `yaml:"upgradePackages"`
	PropagateProxyEnv  bool                       `yaml:"propagateProxyEnv"`
	Video              videoDefinition            `yaml:"video"`
	SSH                sshDefinition              `yaml:"ssh"`
	User               userDefinition             `yaml:"user"`
	AdditionalDisks    []additionalDiskDefinition `yaml:"additionalDisks"`
	PortForwards       []portForwardDefinition    `yaml:"portForwards"`
}

type videoDefinition struct {
	Display string `yaml:"display"`
}

type sshDefinition struct {
	LoadDotSSHPubKeys bool `yaml:"loadDotSSHPubKeys"`
	ForwardAgent      bool `yaml:"forwardAgent"`
	ForwardX11        bool `yaml:"forwardX11"`
	ForwardX11Trusted bool `yaml:"forwardX11Trusted"`
}

type userDefinition struct {
	Name  string `yaml:"name"`
	Home  string `yaml:"home"`
	Shell string `yaml:"shell"`
	UID   uint32 `yaml:"uid"`
}

type imageDefinition struct {
	Location string `yaml:"location"`
	Arch     string `yaml:"arch"`
	Digest   string `yaml:"digest"`
}

type mountDefinition struct{}

type containerdDefinition struct {
	System bool `yaml:"system"`
	User   bool `yaml:"user"`
}

type additionalDiskDefinition struct {
	Name   string `yaml:"name"`
	Format bool   `yaml:"format"`
	FSType string `yaml:"fsType"`
}

type portForwardDefinition struct {
	GuestPort int    `yaml:"guestPort,omitempty"`
	HostPort  int    `yaml:"hostPort,omitempty"`
	GuestIP   string `yaml:"guestIP,omitempty"`
	HostIP    string `yaml:"hostIP,omitempty"`
	Proto     string `yaml:"proto,omitempty"`
	Ignore    bool   `yaml:"ignore,omitempty"`
}
