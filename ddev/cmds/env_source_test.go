package cmds

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBuildSourceArtifactCapturesDirtyTrackedAndExplicitUntracked(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init")
	runGit(t, repository, "config", "user.email", "test@example.com")
	runGit(t, repository, "config", "user.name", "Test")
	writeTestFile(t, filepath.Join(repository, "model.bin"), "pointer")
	runGit(t, repository, "add", "model.bin")
	runGit(t, repository, "commit", "-m", "initial")
	writeTestFile(t, filepath.Join(repository, "model.bin"), "hydrated-model")
	writeTestFile(t, filepath.Join(repository, "new.txt"), "new-file")

	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(repository); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(original)

	if _, err = buildSourceArtifact(false, false); err == nil {
		t.Fatal("untracked files should require an explicit flag")
	}
	artifact, err := buildSourceArtifact(true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Cleanup()
	files := readTestArchive(t, artifact.Path)
	if string(files["worktree/model.bin"]) != "hydrated-model" ||
		string(files["worktree/new.txt"]) != "new-file" || len(files["source.bundle"]) == 0 {
		t.Fatalf("unexpected archive contents: %#v", files)
	}
}

func TestSafeRepositoryURLRejectsCredentials(t *testing.T) {
	if _, err := safeRepositoryURL("https://token@github.com/GetDuranta/app.git"); err == nil {
		t.Fatal("credential-bearing origin was accepted")
	}
	for _, remote := range []string{"https://github.com/GetDuranta/app.git", "git@github.com:GetDuranta/app.git"} {
		if got, err := safeRepositoryURL(remote); err != nil || got == "" {
			t.Fatalf("safe origin rejected: %s: %v", remote, err)
		}
	}
}

func TestBuildSourceArtifactRecordsRenameAsAddAndDelete(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init")
	runGit(t, repository, "config", "user.email", "test@example.com")
	runGit(t, repository, "config", "user.name", "Test")
	writeTestFile(t, filepath.Join(repository, "old.txt"), "content")
	runGit(t, repository, "add", "old.txt")
	runGit(t, repository, "commit", "-m", "initial")
	runGit(t, repository, "mv", "old.txt", "new.txt")

	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(repository); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(original)

	artifact, err := buildSourceArtifact(false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Cleanup()
	files := readTestArchive(t, artifact.Path)
	if string(files["worktree/new.txt"]) != "content" || string(files["deleted"]) != "old.txt\x00" {
		t.Fatalf("unexpected rename archive: %#v", files)
	}
}

func runGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

func writeTestFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	files := map[string][]byte{}
	for {
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			return files
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		content, readErr := io.ReadAll(reader)
		if readErr != nil {
			t.Fatal(readErr)
		}
		files[header.Name] = content
	}
}
