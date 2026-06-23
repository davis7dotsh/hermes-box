package guestupdate

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/davis7dotsh/hermes-box/internal/component"
)

func (e *Engine) BackupStream(ctx context.Context, output io.Writer, requested []string) error {
	return e.backupStream(ctx, output, requested, true)
}

func (e *Engine) SnapshotStream(ctx context.Context, output io.Writer, name component.Name) error {
	requested := component.SnapshotPaths(name)
	if len(requested) == 0 {
		return fmt.Errorf("unknown component snapshot scope %q", name)
	}
	if err := e.checkInteractiveBusy(ctx); err != nil {
		return err
	}
	return e.backupStream(ctx, output, requested, false)
}

func (e *Engine) backupStream(ctx context.Context, output io.Writer, requested []string, quiesceUser bool) error {
	paths, unlock, err := e.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := e.recoverPathRestore(paths); err != nil {
		return err
	}
	if err := ensureNoPendingJournal(paths); err != nil {
		return err
	}
	services, err := e.freeze(ctx)
	if err != nil {
		return err
	}
	thawAgent := func(context.Context) error { return nil }
	agentFrozen := false
	if quiesceUser {
		var err error
		thawAgent, err = e.quiesceAgent(ctx)
		if err != nil {
			cleanup, cancel := cleanupContext()
			_ = e.unfreeze(cleanup, services)
			cancel()
			return err
		}
		agentFrozen = true
	}
	filesystemFrozen := false
	resumed := false
	defer func() {
		cleanup, cancel := cleanupContext()
		defer cancel()
		if filesystemFrozen {
			_, _ = e.Runner.Run(cleanup, []string{"/usr/sbin/fsfreeze", "--unfreeze", paths.data}, RunOptions{Stderr: e.Stderr})
		}
		if agentFrozen {
			_ = thawAgent(cleanup)
		}
		if !resumed {
			_ = e.unfreeze(cleanup, services)
		}
	}()
	if _, err := e.Runner.Run(ctx, []string{"/usr/bin/sync", "--file-system", paths.data}, RunOptions{Stderr: e.Stderr}); err != nil {
		return fmt.Errorf("flush data filesystem: %w", err)
	}
	if _, err := e.Runner.Run(ctx, []string{"/usr/sbin/fsfreeze", "--freeze", paths.data}, RunOptions{Stderr: e.Stderr}); err != nil {
		return fmt.Errorf("freeze data filesystem: %w", err)
	}
	filesystemFrozen = true
	if len(requested) == 0 {
		requested = []string{"home/agent", "executor"}
	}
	written := map[string]bool{}
	archive := tar.NewWriter(contextWriter{Context: ctx, Writer: output})
	if quiesceUser {
		if err := addDataRootHeader(archive, paths.data); err != nil {
			archive.Close()
			return err
		}
	}
	for _, relative := range requested {
		clean, err := safeDataRelative(relative)
		if err != nil {
			archive.Close()
			return err
		}
		if clean == "cache" || strings.HasPrefix(clean, "cache/") {
			archive.Close()
			return errors.New("data cache is excluded from backup")
		}
		source := filepath.Join(paths.data, filepath.FromSlash(clean))
		if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
			header := &tar.Header{Name: "data/" + clean, Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000, PAXRecords: map[string]string{"HERMESBOX.absent": "1"}}
			if err := archive.WriteHeader(header); err != nil {
				archive.Close()
				return err
			}
			continue
		} else if err != nil {
			archive.Close()
			return err
		}
		if err := addArchivePath(ctx, archive, paths.data, source, written); err != nil {
			archive.Close()
			return err
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if _, err := e.Runner.Run(ctx, []string{"/usr/sbin/fsfreeze", "--unfreeze", paths.data}, RunOptions{Stderr: e.Stderr}); err != nil {
		return fmt.Errorf("unfreeze data filesystem: %w", err)
	}
	filesystemFrozen = false
	if agentFrozen {
		if err := thawAgent(ctx); err != nil {
			return err
		}
		agentFrozen = false
	}
	if err := e.unfreeze(ctx, services); err != nil {
		return err
	}
	resumed = true
	return nil
}

func (e *Engine) quiesceAgent(ctx context.Context) (func(context.Context) error, error) {
	if e.Root == "" || e.Root == "/" {
		cgroup, err := os.ReadFile("/proc/self/cgroup")
		if err != nil {
			return nil, fmt.Errorf("inspect backup helper cgroup: %w", err)
		}
		if bytes.Contains(cgroup, []byte("/user.slice/")) {
			return nil, errors.New("backup helper must run in a root system scope before quiescing agent sessions")
		}
	}
	exit, _ := e.Runner.Run(ctx, []string{"systemctl", "is-active", "--quiet", "user-1000.slice"}, RunOptions{})
	if exit != 0 {
		return func(context.Context) error { return nil }, nil
	}
	if _, err := e.Runner.Run(ctx, []string{"systemctl", "freeze", "user-1000.slice"}, RunOptions{Stderr: e.Stderr}); err != nil {
		return nil, fmt.Errorf("freeze agent user slice: %w", err)
	}
	return func(thawContext context.Context) error {
		if _, err := e.Runner.Run(thawContext, []string{"systemctl", "thaw", "user-1000.slice"}, RunOptions{Stderr: e.Stderr}); err != nil {
			return fmt.Errorf("thaw agent user slice: %w", err)
		}
		return nil
	}, nil
}

func (e *Engine) RestoreStream(input io.Reader, replaceExisting bool) (returnErr error) {
	paths, unlock, err := e.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.MkdirAll(paths.data, 0o755); err != nil {
		return err
	}
	if err := e.recoverPathRestore(paths); err != nil {
		return err
	}
	if err := ensureNoPendingJournal(paths); err != nil {
		return err
	}
	staging, err := e.createRestoreStaging(paths, "")
	if err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			panic(recovered)
		}
		if cleanupErr := e.recoverPathRestore(paths); cleanupErr != nil {
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	archive := tar.NewReader(input)
	var dataRootHeader *tar.Header
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Clean(header.Name))
		if name == "data" {
			if dataRootHeader != nil || (header.Name != "data" && header.Name != "data/") {
				return errors.New("archive must contain exactly one normalized data root header")
			}
			if header.Typeflag != tar.TypeDir || header.Size != 0 || !safeArchiveIdentity(header) {
				return errors.New("archive data root must be a safe directory header")
			}
			if _, err := archiveMode(header.Mode); err != nil {
				return fmt.Errorf("archive data root mode: %w", err)
			}
			copy := *header
			dataRootHeader = &copy
			continue
		}
		if dataRootHeader == nil {
			return errors.New("archive data root header must precede data entries")
		}
		if !strings.HasPrefix(name, "data/") {
			return fmt.Errorf("archive path is outside data: %q", header.Name)
		}
		relative, err := safeDataRelative(strings.TrimPrefix(name, "data/"))
		if err != nil {
			return err
		}
		if relative == "cache" || strings.HasPrefix(relative, "cache/") {
			return errors.New("archive may not restore data cache")
		}
		if !safeArchiveIdentity(header) {
			return fmt.Errorf("archive ownership is invalid for %q", header.Name)
		}
		target := filepath.Join(staging, filepath.FromSlash(relative))
		if !inside(staging, target) {
			return fmt.Errorf("archive path escapes restore root: %q", header.Name)
		}
		if err := rejectSymlinkAncestors(staging, target); err != nil {
			return fmt.Errorf("archive path %q: %w", header.Name, err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			mode, err := archiveMode(header.Mode)
			if err != nil {
				return fmt.Errorf("archive mode for %q: %w", header.Name, err)
			}
			if err := os.MkdirAll(target, mode.Perm()); err != nil {
				return err
			}
			if err := e.restoreMetadata(target, header, false); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			mode, err := archiveMode(header.Mode)
			if err != nil {
				return fmt.Errorf("archive mode for %q: %w", header.Name, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
			if err != nil {
				return err
			}
			if _, err := io.CopyN(file, archive, header.Size); err != nil {
				file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
			if err := e.restoreMetadata(target, header, false); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(header.Linkname) || strings.ContainsRune(header.Linkname, '\x00') {
				return fmt.Errorf("absolute symlink rejected: %q", header.Name)
			}
			resolved := filepath.Join(filepath.Dir(target), filepath.FromSlash(header.Linkname))
			if !inside(staging, resolved) {
				return fmt.Errorf("symlink escapes restored data: %q", header.Name)
			}
			if err := rejectSymlinkAncestors(staging, resolved); err != nil {
				return fmt.Errorf("symlink target for %q: %w", header.Name, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
			if err := e.restoreMetadata(target, header, true); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry type for %q", header.Name)
		}
		e.durabilityCheckpoint("extraction-entry:" + name)
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	if dataRootHeader == nil {
		return errors.New("archive is missing the data root header")
	}
	if len(entries) != 2 || !directoryExists(filepath.Join(staging, "home")) || !directoryExists(filepath.Join(staging, "executor")) {
		return errors.New("restore archive must contain exactly data/home and data/executor")
	}
	if !replaceExisting {
		for _, entry := range entries {
			destination := filepath.Join(paths.data, entry.Name())
			if _, err := os.Lstat(destination); err == nil {
				empty, emptyErr := emptyTree(destination)
				if emptyErr != nil {
					return emptyErr
				}
				if !empty {
					return fmt.Errorf("restore destination already contains durable state: %s", destination)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if err := syncTree(staging); err != nil {
		return fmt.Errorf("sync staged restore: %w", err)
	}
	scopes := make([]string, 0, len(entries))
	for _, entry := range entries {
		scopes = append(scopes, entry.Name())
	}
	remount := false
	if e.Root == "" || e.Root == "/" {
		if err := os.Chdir("/"); err != nil {
			return err
		}
		operation, cancel := cleanupContext()
		_, unmountErr := e.Runner.Run(operation, []string{"/usr/bin/umount", "/home/agent"}, RunOptions{Stderr: e.Stderr})
		cancel()
		if unmountErr != nil {
			return fmt.Errorf("unmount persistent home for restore: %w", unmountErr)
		}
		remount = true
	}
	defer func() {
		if remount {
			cleanup, cancel := cleanupContext()
			defer cancel()
			_, _ = e.Runner.Run(cleanup, []string{"/usr/bin/mount", "/home/agent"}, RunOptions{Stderr: e.Stderr})
		}
	}()
	if err := e.publishScopedRestore(paths, staging, "", scopes, nil, dataRootHeader); err != nil {
		return err
	}
	if remount {
		operation, cancel := cleanupContext()
		_, mountErr := e.Runner.Run(operation, []string{"/usr/bin/mount", "/home/agent"}, RunOptions{Stderr: e.Stderr})
		cancel()
		if mountErr != nil {
			return fmt.Errorf("remount restored persistent home: %w", mountErr)
		}
		remount = false
	}
	return nil
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// RestorePaths replaces only the centrally defined durable snapshot scope for
// one component. It is the host-coordinated half of update rollback: the host
// decrypts the retained snapshot, this method publishes its paths, and only
// then may the host request Recover or Rollback.
func (e *Engine) RestorePaths(ctx context.Context, name component.Name, input io.Reader) (returnErr error) {
	scopes := component.SnapshotPaths(name)
	if len(scopes) == 0 {
		return fmt.Errorf("unknown component snapshot scope %q", name)
	}
	paths, unlock, err := e.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := e.recoverPathRestore(paths); err != nil {
		return err
	}
	staging, err := e.createRestoreStaging(paths, name)
	if err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			panic(recovered)
		}
		if cleanupErr := e.recoverPathRestore(paths); cleanupErr != nil {
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	seen := make(map[string]bool, len(scopes))
	absent := make(map[string]bool, len(scopes))
	archive := tar.NewReader(input)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Clean(header.Name))
		if !strings.HasPrefix(name, "data/") {
			return fmt.Errorf("snapshot path is outside data: %q", header.Name)
		}
		relative, err := safeDataRelative(strings.TrimPrefix(name, "data/"))
		if err != nil || !withinScopes(relative, scopes) {
			return fmt.Errorf("snapshot path is outside %s scope: %q", name, header.Name)
		}
		for _, scope := range scopes {
			if relative == scope {
				seen[scope] = true
				absent[scope] = header.PAXRecords["HERMESBOX.absent"] == "1"
			}
		}
		target := filepath.Join(staging, filepath.FromSlash(relative))
		if header.Uid < 0 || header.Uid > 65535 || header.Gid < 0 || header.Gid > 65535 {
			return fmt.Errorf("snapshot ownership is invalid for %q", header.Name)
		}
		if err := rejectSymlinkAncestors(staging, target); err != nil {
			return fmt.Errorf("snapshot path %q: %w", header.Name, err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			mode, err := archiveMode(header.Mode)
			if err != nil {
				return fmt.Errorf("snapshot mode for %q: %w", header.Name, err)
			}
			if err := os.MkdirAll(target, mode.Perm()); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			mode, err := archiveMode(header.Mode)
			if err != nil {
				return fmt.Errorf("snapshot mode for %q: %w", header.Name, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
			if err != nil {
				return err
			}
			if _, err := io.CopyN(file, archive, header.Size); err != nil {
				file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(header.Linkname) || strings.ContainsRune(header.Linkname, '\x00') {
				return fmt.Errorf("absolute snapshot symlink rejected: %q", header.Name)
			}
			resolved := filepath.Join(filepath.Dir(target), filepath.FromSlash(header.Linkname))
			resolvedRelative, err := filepath.Rel(staging, resolved)
			if err != nil || !withinScopes(filepath.ToSlash(resolvedRelative), scopes) {
				return fmt.Errorf("snapshot symlink escapes component scope: %q", header.Name)
			}
			if err := rejectSymlinkAncestors(staging, resolved); err != nil {
				return fmt.Errorf("snapshot symlink target for %q: %w", header.Name, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported snapshot entry type for %q", header.Name)
		}
		if err := e.restoreMetadata(target, header, header.Typeflag == tar.TypeSymlink); err != nil {
			return err
		}
		e.durabilityCheckpoint("extraction-entry:" + relative)
	}
	for _, scope := range scopes {
		if !seen[scope] {
			return fmt.Errorf("snapshot is missing required scope %s", scope)
		}
		if absent[scope] {
			if err := os.RemoveAll(filepath.Join(staging, filepath.FromSlash(scope))); err != nil {
				return err
			}
		}
	}
	if err := syncTree(staging); err != nil {
		return fmt.Errorf("sync staged component restore: %w", err)
	}
	services, err := e.freeze(ctx)
	if err != nil {
		return err
	}
	resume := !fileExists(paths.journal)
	servicesResumed := !resume
	defer func() {
		if !servicesResumed {
			cleanup, cancel := cleanupContext()
			defer cancel()
			_ = e.unfreeze(cleanup, services)
		}
	}()
	if err := e.publishScopedRestore(paths, staging, name, scopes, absent, nil); err != nil {
		return err
	}
	if resume {
		if err := e.unfreeze(ctx, services); err != nil {
			return err
		}
		servicesResumed = true
	}
	return nil
}

func (e *Engine) publishScopedRestore(paths paths, staging string, name component.Name, scopes []string, absent map[string]bool, dataRootHeader *tar.Header) error {
	if err := os.MkdirAll(filepath.Dir(paths.restoreJournal), 0o700); err != nil {
		return err
	}
	var journal pathRestoreJournal
	if err := readJSON(paths.restoreJournal, &journal); err != nil {
		return err
	}
	if journal.Schema != ProtocolSchema || journal.Phase != "extracting" || journal.Component != name || journal.Staging != staging {
		return errors.New("restore extraction journal does not own the staged tree")
	}
	journal.Phase = "publishing"
	journal.Entries = make([]pathRestoreEntry, 0, len(scopes))
	if dataRootHeader != nil {
		before, err := metadataForPath(paths.data)
		if err != nil {
			return err
		}
		after, err := metadataForHeader(dataRootHeader)
		if err != nil {
			return err
		}
		journal.DataRoot = &pathRestoreRoot{Before: before, After: after}
	}
	for index, scope := range scopes {
		destination := filepath.Join(paths.data, filepath.FromSlash(scope))
		_, err := os.Lstat(destination)
		journal.Entries = append(journal.Entries, pathRestoreEntry{
			Scope: scope, Destination: destination,
			Old:         destination + fmt.Sprintf(".hermes-box-old-%d-%d", os.Getpid(), index),
			HadOriginal: err == nil,
			Absent:      absent[scope],
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := e.atomicJSON(paths.restoreJournal, journal, 0o600); err != nil {
		return err
	}
	e.durabilityCheckpoint("journal-durable")
	for index := range journal.Entries {
		entry := &journal.Entries[index]
		if err := os.MkdirAll(filepath.Dir(entry.Destination), 0o700); err != nil {
			_ = e.recoverPathRestore(paths)
			return err
		}
		if entry.HadOriginal {
			if err := durableRename(entry.Destination, entry.Old); err != nil {
				_ = e.recoverPathRestore(paths)
				return err
			}
			e.durabilityCheckpoint("old-durable:" + entry.Scope)
		}
		if !entry.Absent {
			if err := durableRename(filepath.Join(staging, filepath.FromSlash(entry.Scope)), entry.Destination); err != nil {
				_ = e.recoverPathRestore(paths)
				return err
			}
			e.durabilityCheckpoint("candidate-durable:" + entry.Scope)
		}
		entry.Installed = true
		if err := e.atomicJSON(paths.restoreJournal, journal, 0o600); err != nil {
			_ = e.recoverPathRestore(paths)
			return err
		}
	}
	if journal.DataRoot != nil {
		if err := e.applyMetadata(paths.data, journal.DataRoot.After, false); err != nil {
			_ = e.recoverPathRestore(paths)
			return fmt.Errorf("restore data root metadata: %w", err)
		}
		if err := syncDirectory(paths.data); err != nil {
			_ = e.recoverPathRestore(paths)
			return fmt.Errorf("sync restored data root metadata: %w", err)
		}
	}
	journal.Committed = true
	if err := e.atomicJSON(paths.restoreJournal, journal, 0o600); err != nil {
		_ = e.recoverPathRestore(paths)
		return err
	}
	e.durabilityCheckpoint("commit-durable")
	return e.recoverPathRestore(paths)
}

func (e *Engine) recoverPathRestore(paths paths) error {
	var journal pathRestoreJournal
	if err := readJSON(paths.restoreJournal, &journal); err != nil {
		return err
	}
	if journal.Schema == 0 {
		return nil
	}
	if err := validatePathRestoreJournal(paths, journal); err != nil {
		return err
	}
	if journal.Phase == "extracting" {
		if err := durableRemove(journal.Staging, true); err != nil {
			return err
		}
		return durableRemove(paths.restoreJournal, false)
	}
	if journal.Committed && journal.DataRoot != nil {
		if err := e.applyMetadata(paths.data, journal.DataRoot.After, false); err != nil {
			return fmt.Errorf("finish restored data root metadata: %w", err)
		}
	}
	for index := len(journal.Entries) - 1; index >= 0; index-- {
		entry := journal.Entries[index]
		if journal.Committed {
			_, destinationErr := os.Lstat(entry.Destination)
			if entry.Absent {
				if destinationErr == nil {
					return fmt.Errorf("committed restore destination should be absent; preserving prior copy: %s", entry.Destination)
				}
				if !errors.Is(destinationErr, os.ErrNotExist) {
					return destinationErr
				}
			} else {
				if destinationErr != nil {
					if errors.Is(destinationErr, os.ErrNotExist) {
						return fmt.Errorf("committed restore destination is missing; preserving prior copy: %s", entry.Destination)
					}
					return destinationErr
				}
			}
			if err := durableRemove(entry.Old, true); err != nil {
				return err
			}
			continue
		}
		if entry.HadOriginal {
			if _, err := os.Lstat(entry.Old); err == nil {
				if err := durableRemove(entry.Destination, true); err != nil {
					return err
				}
				if err := durableRename(entry.Old, entry.Destination); err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			} else if entry.Installed {
				return fmt.Errorf("uncommitted restore lost its prior copy: %s", entry.Old)
			}
		} else {
			if err := durableRemove(entry.Destination, true); err != nil {
				return err
			}
		}
	}
	if !journal.Committed && journal.DataRoot != nil {
		if err := e.applyMetadata(paths.data, journal.DataRoot.Before, false); err != nil {
			return fmt.Errorf("recover data root metadata: %w", err)
		}
		if err := syncDirectory(paths.data); err != nil {
			return fmt.Errorf("sync recovered data root metadata: %w", err)
		}
	}
	if err := durableRemove(journal.Staging, true); err != nil {
		return err
	}
	return durableRemove(paths.restoreJournal, false)
}

func validatePathRestoreJournal(paths paths, journal pathRestoreJournal) error {
	if journal.Schema != ProtocolSchema {
		return errors.New("path restore journal has an unsupported schema")
	}
	prefix := ".restore-"
	if journal.Component != "" {
		prefix = ".restore-paths-"
	}
	stagingPattern := regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `[0-9a-f]{32}$`)
	if filepath.Dir(journal.Staging) != paths.data || !stagingPattern.MatchString(filepath.Base(journal.Staging)) || !inside(paths.data, journal.Staging) {
		return errors.New("path restore journal has an invalid staging path")
	}
	if journal.Phase == "extracting" {
		if journal.Committed || journal.DataRoot != nil || len(journal.Entries) != 0 {
			return errors.New("restore extraction journal contains publication state")
		}
		return nil
	}
	if journal.Phase != "publishing" {
		return errors.New("path restore journal has an invalid phase")
	}
	allowed := component.SnapshotPaths(journal.Component)
	if journal.Component == "" {
		allowed = []string{"home", "executor"}
	}
	if len(allowed) == 0 {
		return errors.New("path restore journal has an invalid component")
	}
	if len(journal.Entries) != len(allowed) {
		return errors.New("path restore journal has an invalid scope count")
	}
	if journal.Component != "" && journal.DataRoot != nil {
		return errors.New("component path restore journal may not change the data root")
	}
	if journal.Component == "" && journal.DataRoot == nil {
		return errors.New("full restore journal is missing data root metadata")
	}
	if journal.DataRoot != nil {
		if err := validateArchiveMetadata(journal.DataRoot.Before); err != nil {
			return fmt.Errorf("path restore journal has invalid prior data root metadata: %w", err)
		}
		if err := validateArchiveMetadata(journal.DataRoot.After); err != nil {
			return fmt.Errorf("path restore journal has invalid restored data root metadata: %w", err)
		}
	}
	seen := make(map[string]bool, len(journal.Entries))
	for _, entry := range journal.Entries {
		oldPattern := regexp.MustCompile(`^` + regexp.QuoteMeta(entry.Destination) + `\.hermes-box-old-[0-9]+-[0-9]+$`)
		if seen[entry.Scope] || !slices.Contains(allowed, entry.Scope) || entry.Destination != filepath.Join(paths.data, filepath.FromSlash(entry.Scope)) || !oldPattern.MatchString(entry.Old) || !inside(paths.data, entry.Destination) || !inside(paths.data, entry.Old) {
			return errors.New("path restore journal escapes data")
		}
		seen[entry.Scope] = true
	}
	return nil
}

func (e *Engine) createRestoreStaging(paths paths, name component.Name) (string, error) {
	prefix := ".restore-"
	if name != "" {
		prefix = ".restore-paths-"
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	staging := filepath.Join(paths.data, prefix+hex.EncodeToString(random))
	journal := pathRestoreJournal{
		Schema: ProtocolSchema, Component: name, Phase: "extracting", Staging: staging,
	}
	if err := os.MkdirAll(filepath.Dir(paths.restoreJournal), 0o700); err != nil {
		return "", err
	}
	if err := e.atomicJSON(paths.restoreJournal, journal, 0o600); err != nil {
		return "", err
	}
	// atomicJSON syncs the journal and its .hermes-box directory. Sync /data
	// as a separate durability boundary so a newly created .hermes-box entry
	// is recoverable after power loss before any staging directory can exist.
	if err := e.syncDirectory(paths.data); err != nil {
		_ = durableRemove(paths.restoreJournal, false)
		return "", err
	}
	e.durabilityCheckpoint("extraction-journal-durable")
	if err := os.Mkdir(staging, 0o700); err != nil {
		// Mkdir is the ownership boundary. On failure the proposed random path
		// was never ours, so clear only the journal and never remove that path.
		_ = durableRemove(paths.restoreJournal, false)
		return "", err
	}
	if err := e.syncDirectory(paths.data); err != nil {
		_ = e.recoverPathRestore(paths)
		return "", err
	}
	e.durabilityCheckpoint("staging-durable")
	return staging, nil
}

func withinScopes(relative string, scopes []string) bool {
	for _, scope := range scopes {
		if relative == scope || strings.HasPrefix(relative, scope+"/") {
			return true
		}
	}
	return false
}

func emptyTree(path string) (bool, error) {
	empty := true
	err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current != path && !entry.IsDir() {
			empty = false
			return fs.SkipAll
		}
		return nil
	})
	return empty, err
}

func addArchivePath(ctx context.Context, archive *tar.Writer, dataRoot, source string, written map[string]bool) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(dataRoot, path)
		if err != nil {
			return err
		}
		name := "data/" + filepath.ToSlash(relative)
		if written[name] {
			return nil
		}
		written[name] = true
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) || !inside(dataRoot, filepath.Join(filepath.Dir(path), link)) {
				return fmt.Errorf("backup symlink escapes data: %s", path)
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = name
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(archive, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		}
		return nil
	})
}

type contextWriter struct {
	Context context.Context
	Writer  io.Writer
}

func (writer contextWriter) Write(value []byte) (int, error) {
	if err := writer.Context.Err(); err != nil {
		return 0, err
	}
	return writer.Writer.Write(value)
}

func addDataRootHeader(archive *tar.Writer, dataRoot string) error {
	info, err := os.Lstat(dataRoot)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("data root is not a directory")
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = "data"
	return archive.WriteHeader(header)
}

func (e *Engine) restoreMetadata(path string, header *tar.Header, symlink bool) error {
	metadata, err := metadataForHeader(header)
	if err != nil {
		return err
	}
	return e.applyMetadata(path, metadata, symlink)
}

func (e *Engine) applyMetadata(path string, metadata archiveMetadata, symlink bool) error {
	if err := validateArchiveMetadata(metadata); err != nil {
		return err
	}
	if e.Root == "" || e.Root == "/" {
		if symlink {
			return os.Lchown(path, metadata.UID, metadata.GID)
		}
		if err := os.Chown(path, metadata.UID, metadata.GID); err != nil {
			return err
		}
	}
	if symlink {
		return nil
	}
	mode, err := archiveMode(metadata.Mode)
	if err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func metadataForHeader(header *tar.Header) (archiveMetadata, error) {
	metadata := archiveMetadata{Mode: header.Mode, UID: header.Uid, GID: header.Gid}
	return metadata, validateArchiveMetadata(metadata)
}

func metadataForPath(path string) (archiveMetadata, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return archiveMetadata{}, err
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return archiveMetadata{}, err
	}
	return metadataForHeader(header)
}

func safeArchiveIdentity(header *tar.Header) bool {
	return header.Uid >= 0 && header.Uid <= 65535 && header.Gid >= 0 && header.Gid <= 65535
}

func validateArchiveMetadata(metadata archiveMetadata) error {
	if metadata.UID < 0 || metadata.UID > 65535 || metadata.GID < 0 || metadata.GID > 65535 {
		return errors.New("uid and gid must be in 0..65535")
	}
	_, err := archiveMode(metadata.Mode)
	return err
}

func archiveMode(value int64) (fs.FileMode, error) {
	if value < 0 || value & ^int64(0o7777) != 0 {
		return 0, fmt.Errorf("mode %o is outside 0000..07777", value)
	}
	mode := fs.FileMode(value & 0o777)
	if value&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}
	if value&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if value&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	return mode, nil
}

func rejectSymlinkAncestors(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("path escapes restore root")
	}
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	current := root
	for _, part := range parts[:max(0, len(parts)-1)] {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path is beneath symlink ancestor %s", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("path has non-directory ancestor %s", current)
		}
	}
	return nil
}

func safeDataRelative(value string) (string, error) {
	if value == "" || filepath.IsAbs(value) || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("invalid relative data path %q", value)
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid relative data path %q", value)
	}
	return clean, nil
}

func inside(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
