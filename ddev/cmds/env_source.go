package cmds

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type sourceArtifact struct {
	Path       string
	Root       string
	Commit     string
	Repository string
	Cleanup    func()
}

func buildSourceArtifact(includeUntracked, includeLFS bool) (sourceArtifact, error) {
	root, err := gitOutput("rev-parse", "--show-toplevel")
	if err != nil {
		return sourceArtifact{}, err
	}
	commit, err := gitOutputAt(root, "rev-parse", "HEAD")
	if err != nil {
		return sourceArtifact{}, err
	}
	repository, err := gitOutputAt(root, "remote", "get-url", "origin")
	if err != nil || repository == "" {
		repository = "local:" + filepath.Base(root)
	} else if repository, err = safeRepositoryURL(repository); err != nil {
		return sourceArtifact{}, err
	}
	temporary, err := os.MkdirTemp("", "ddev-env-source-")
	if err != nil {
		return sourceArtifact{}, err
	}
	cleanup := func() { _ = os.RemoveAll(temporary) }
	bundle := filepath.Join(temporary, "source.bundle")
	command := exec.Command("git", "bundle", "create", bundle, "HEAD")
	command.Dir = root
	if output, commandErr := command.CombinedOutput(); commandErr != nil {
		cleanup()
		return sourceArtifact{}, fmt.Errorf("create git bundle: %w: %s", commandErr, strings.TrimSpace(string(output)))
	}
	tracked, err := gitPaths(root, "diff", "--no-renames", "--name-only", "--diff-filter=ACMRTUXB", "-z", "HEAD")
	if err != nil {
		cleanup()
		return sourceArtifact{}, err
	}
	untracked, err := gitPaths(root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		cleanup()
		return sourceArtifact{}, err
	}
	if len(untracked) > 0 && !includeUntracked {
		cleanup()
		return sourceArtifact{}, fmt.Errorf("checkout has %d untracked files; review them, then stage them or use --include-untracked", len(untracked))
	}
	if includeUntracked {
		tracked = append(tracked, untracked...)
	}
	lfsFiles, lfsErr := gitPaths(root, "lfs", "ls-files", "--name-only", "-z")
	if includeLFS {
		if lfsErr != nil {
			cleanup()
			return sourceArtifact{}, fmt.Errorf("list hydrated Git LFS files: %w", lfsErr)
		}
		tracked = append(tracked, lfsFiles...)
	} else if lfsErr == nil {
		tracked = withoutPaths(tracked, lfsFiles)
	}
	deleted, err := gitPaths(root, "diff", "--no-renames", "--name-only", "--diff-filter=D", "-z", "HEAD")
	if err != nil {
		cleanup()
		return sourceArtifact{}, err
	}
	artifactPath := filepath.Join(temporary, "source.tgz")
	if err = writeSourceArchive(artifactPath, bundle, root, tracked, deleted); err != nil {
		cleanup()
		return sourceArtifact{}, err
	}
	return sourceArtifact{
		Path: artifactPath, Root: root, Commit: commit, Repository: repository, Cleanup: cleanup,
	}, nil
}

func safeRepositoryURL(raw string) (string, error) {
	if strings.HasPrefix(raw, "git@") && strings.Contains(raw, ":") {
		return raw, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "ssh") || parsed.Host == "" {
		return "", errors.New("origin must be an HTTPS or SSH Git URL")
	}
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword || parsed.Scheme == "https" {
			return "", errors.New("origin URL contains credentials; configure a credential-free remote")
		}
	}
	return parsed.String(), nil
}

func writeSourceArchive(target, bundle, root string, paths, deleted []string) (returnErr error) {
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); returnErr == nil {
			returnErr = err
		}
	}()
	gzipWriter, err := gzip.NewWriterLevel(file, gzip.BestSpeed)
	if err != nil {
		return err
	}
	defer func() {
		if err := gzipWriter.Close(); returnErr == nil {
			returnErr = err
		}
	}()
	tarWriter := tar.NewWriter(gzipWriter)
	defer func() {
		if err := tarWriter.Close(); returnErr == nil {
			returnErr = err
		}
	}()
	if err = addArchiveFile(tarWriter, bundle, "source.bundle"); err != nil {
		return err
	}
	if err = tarWriter.WriteHeader(&tar.Header{Name: "worktree/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		return err
	}
	for _, path := range uniquePaths(paths) {
		if !safeRelativePath(path) {
			return fmt.Errorf("unsafe Git path %q", path)
		}
		if err = addArchiveFile(tarWriter, filepath.Join(root, path), filepath.ToSlash(filepath.Join("worktree", path))); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("archive %s: %w", path, err)
		}
	}
	deletedRaw := []byte(strings.Join(deleted, "\x00"))
	if len(deletedRaw) > 0 {
		deletedRaw = append(deletedRaw, 0)
	}
	return writeTarBytes(tarWriter, "deleted", deletedRaw, 0o600)
}

func addArchiveFile(writer *tar.Writer, source, name string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = filepath.ToSlash(name)
	if info.Mode()&os.ModeSymlink != 0 {
		target, readErr := os.Readlink(source)
		if readErr != nil {
			return readErr
		}
		header.Linkname = target
	}
	if err = writer.WriteHeader(header); err != nil || !info.Mode().IsRegular() {
		return err
	}
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(writer, file)
	return err
}

func writeTarBytes(writer *tar.Writer, name string, value []byte, mode int64) error {
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(value))}); err != nil {
		return err
	}
	_, err := writer.Write(value)
	return err
}

func gitPaths(root string, args ...string) ([]string, error) {
	command := exec.Command("git", args...)
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	parts := strings.Split(string(output), "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts, nil
}

func gitOutput(args ...string) (string, error) {
	return gitOutputAt("", args...)
}

func gitOutputAt(directory string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func safeRelativePath(path string) bool {
	return path != "" && !filepath.IsAbs(path) && path != "." && path != ".." &&
		!strings.HasPrefix(filepath.Clean(path), ".."+string(filepath.Separator))
}

func uniquePaths(paths []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, found := seen[path]; found {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}

func withoutPaths(paths, excluded []string) []string {
	blocked := make(map[string]struct{}, len(excluded))
	for _, path := range excluded {
		blocked[path] = struct{}{}
	}
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, found := blocked[path]; !found {
			result = append(result, path)
		}
	}
	return result
}
