package devenv

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type sourceTarEntry struct {
	name, link string
	typeflag   byte
	value      string
}

func TestSourceExtractorAcceptsValidArchiveAndAppliesOverlay(t *testing.T) {
	directory := t.TempDir()
	script := writeExtractorScript(t, directory)
	archive := writeSourceTar(t, directory, []sourceTarEntry{
		{name: "source.bundle", typeflag: tar.TypeReg, value: "bundle"},
		{name: "worktree/", typeflag: tar.TypeDir},
		{name: "worktree/bin/run", typeflag: tar.TypeReg, value: "updated"},
		{name: "deleted", typeflag: tar.TypeReg, value: "old.txt\x00"},
	})
	extracted := filepath.Join(directory, "source")
	runExtractor(t, script, "extract", archive, extracted, "1048576")

	repository := filepath.Join(directory, "repo")
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	runExtractor(t, script, "apply", filepath.Join(extracted, "worktree"),
		filepath.Join(extracted, "deleted"), repository)
	value, err := os.ReadFile(filepath.Join(repository, "bin", "run"))
	if err != nil || string(value) != "updated" {
		t.Fatalf("applied file = %q, %v", value, err)
	}
	if _, err = os.Lstat(filepath.Join(repository, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted file still exists: %v", err)
	}
}

func TestSourceExtractorRejectsUnsafeMembers(t *testing.T) {
	tests := map[string][]sourceTarEntry{
		"traversal": {{name: "../escape", typeflag: tar.TypeReg, value: "x"}},
		"hardlink":  {{name: "worktree/link", typeflag: tar.TypeLink, link: "worktree/file"}},
		"device":    {{name: "worktree/device", typeflag: tar.TypeChar}},
		"escaping symlink": {
			{name: "worktree/link", typeflag: tar.TypeSymlink, link: "../../etc/passwd"},
		},
		"symlink ancestor": {
			{name: "worktree/link", typeflag: tar.TypeSymlink, link: "inside"},
			{name: "worktree/link/file", typeflag: tar.TypeReg, value: "x"},
		},
	}
	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			script := writeExtractorScript(t, directory)
			archive := writeSourceTar(t, directory, entries)
			command := exec.Command("python3", script, "extract", archive, filepath.Join(directory, "out"), "1048576")
			if output, err := command.CombinedOutput(); err == nil {
				t.Fatalf("unsafe archive accepted: %s", output)
			}
		})
	}
}

func TestSourceExtractorEnforcesDecompressedLimit(t *testing.T) {
	directory := t.TempDir()
	script := writeExtractorScript(t, directory)
	archive := writeSourceTar(t, directory, []sourceTarEntry{
		{name: "source.bundle", typeflag: tar.TypeReg, value: strings.Repeat("x", 4096)},
		{name: "worktree/", typeflag: tar.TypeDir},
		{name: "deleted", typeflag: tar.TypeReg},
	})
	command := exec.Command("python3", script, "extract", archive, filepath.Join(directory, "out"), "2048")
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("oversized archive accepted: %s", output)
	}
}

func TestSourceOverlayDoesNotFollowRepositorySymlink(t *testing.T) {
	directory := t.TempDir()
	script := writeExtractorScript(t, directory)
	overlay := filepath.Join(directory, "overlay")
	repository := filepath.Join(directory, "repo")
	outside := filepath.Join(directory, "outside")
	for _, path := range []string{filepath.Join(overlay, "linked"), filepath.Join(repository, ".git"), outside} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(overlay, "linked", "file"), []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repository, "linked")); err != nil {
		t.Fatal(err)
	}
	deleted := filepath.Join(directory, "deleted")
	if err := os.WriteFile(deleted, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runExtractor(t, script, "apply", overlay, deleted, repository)
	if _, err := os.Lstat(filepath.Join(outside, "file")); !os.IsNotExist(err) {
		t.Fatalf("overlay escaped repository: %v", err)
	}
	if value, err := os.ReadFile(filepath.Join(repository, "linked", "file")); err != nil || string(value) != "safe" {
		t.Fatalf("safe replacement failed: %q, %v", value, err)
	}
}

func writeExtractorScript(t *testing.T, directory string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is unavailable")
	}
	path := filepath.Join(directory, "source_extract.py")
	if err := os.WriteFile(path, []byte(sourceExtractorScript), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeSourceTar(t *testing.T, directory string, entries []sourceTarEntry) string {
	t.Helper()
	path := filepath.Join(directory, "source.tgz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	writer := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		header := &tar.Header{
			Name: entry.name, Linkname: entry.link, Typeflag: entry.typeflag,
			Mode: 0o755, Size: int64(len(entry.value)),
		}
		if entry.typeflag != tar.TypeReg {
			header.Size = 0
		}
		if err = writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err = writer.Write([]byte(entry.value)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func runExtractor(t *testing.T, script string, arguments ...string) {
	t.Helper()
	command := exec.Command("python3", append([]string{script}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("extractor failed: %v: %s", err, output)
	}
}
