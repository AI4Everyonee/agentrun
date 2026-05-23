package gitmeta

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// mustRun is a helper that runs a command in dir and fails the test on error.
func mustRun(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
}

// TestCapture_FreshRepo tests Capture in a newly initialised repo with one commit.
func TestCapture_FreshRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmp := t.TempDir()

	// Initialise repo with an explicit branch name so the test is reproducible
	// regardless of the host's init.defaultBranch setting.
	mustRun(t, tmp, "git", "init", "-b", "main")
	mustRun(t, tmp, "git", "config", "user.email", "test@example.com")
	mustRun(t, tmp, "git", "config", "user.name", "Test User")

	// Create a file and commit it so HEAD exists.
	if err := os.WriteFile(filepath.Join(tmp, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustRun(t, tmp, "git", "add", "hello.txt")
	mustRun(t, tmp, "git", "commit", "-m", "initial commit")

	snap := Capture(tmp)

	// RepoRoot: resolve symlinks on both sides (macOS maps /var -> /private/var).
	resolvedTmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatalf("EvalSymlinks(tmp): %v", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(snap.RepoRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(snap.RepoRoot=%q): %v", snap.RepoRoot, err)
	}
	if resolvedRoot != resolvedTmp {
		t.Errorf("RepoRoot: got %q, want %q (after symlink resolution)", resolvedRoot, resolvedTmp)
	}

	// Branch should be "main".
	if snap.Branch != "main" {
		t.Errorf("Branch: got %q, want %q", snap.Branch, "main")
	}

	// HeadSHA should be a 40-char lowercase hex string.
	shaRe := regexp.MustCompile(`^[0-9a-f]{40}$`)
	if !shaRe.MatchString(snap.HeadSHA) {
		t.Errorf("HeadSHA: got %q, want 40-char hex", snap.HeadSHA)
	}
}

// TestCapture_NonRepo tests that Capture in a directory that is not a git repo
// returns a zero Snapshot without panicking.
func TestCapture_NonRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmp := t.TempDir()
	snap := Capture(tmp)

	if snap.RepoRoot != "" {
		t.Errorf("RepoRoot: got %q, want empty string", snap.RepoRoot)
	}
	if snap.Branch != "" {
		t.Errorf("Branch: got %q, want empty string", snap.Branch)
	}
	if snap.HeadSHA != "" {
		t.Errorf("HeadSHA: got %q, want empty string", snap.HeadSHA)
	}

	// HeadCommit must also return "" without panic.
	if got := HeadCommit(tmp); got != "" {
		t.Errorf("HeadCommit: got %q, want empty string", got)
	}
}

// TestHeadCommit_MatchesCapture verifies that HeadCommit(repo) == Capture(repo).HeadSHA.
func TestHeadCommit_MatchesCapture(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmp := t.TempDir()

	mustRun(t, tmp, "git", "init", "-b", "main")
	mustRun(t, tmp, "git", "config", "user.email", "test@example.com")
	mustRun(t, tmp, "git", "config", "user.name", "Test User")

	if err := os.WriteFile(filepath.Join(tmp, "file.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustRun(t, tmp, "git", "add", "file.txt")
	mustRun(t, tmp, "git", "commit", "-m", "test commit")

	snap := Capture(tmp)
	head := HeadCommit(tmp)

	if head != snap.HeadSHA {
		t.Errorf("HeadCommit()=%q != Capture().HeadSHA=%q", head, snap.HeadSHA)
	}
	if head == "" {
		t.Error("HeadCommit: got empty string in a valid repo with a commit")
	}
}
