package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Test 1: AGENTRUN_DB_DIR env wins and the values are derived correctly.
func TestLoad_EnvWins(t *testing.T) {
	t.Setenv("AGENTRUN_DB_DIR", "/tmp/some_abs_path")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.DBDir != "/tmp/some_abs_path" {
		t.Errorf("DBDir = %q, want %q", cfg.DBDir, "/tmp/some_abs_path")
	}
	if cfg.DBPath != "/tmp/some_abs_path/agentrun.db" {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, "/tmp/some_abs_path/agentrun.db")
	}
	if cfg.ArtifactsDir != "/tmp/some_abs_path/artifacts" {
		t.Errorf("ArtifactsDir = %q, want %q", cfg.ArtifactsDir, "/tmp/some_abs_path/artifacts")
	}
}

// Test 2: A relative AGENTRUN_DB_DIR is absolutized.
func TestLoad_EnvRelativePathAbsolutized(t *testing.T) {
	t.Setenv("AGENTRUN_DB_DIR", ".myagentrun")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if !filepath.IsAbs(cfg.DBDir) {
		t.Errorf("DBDir = %q is not absolute", cfg.DBDir)
	}
}

// Test 3: Inside a git repo (no AGENTRUN_DB_DIR), DBDir is <repo_root>/.agentrun.
func TestLoad_InGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmp := t.TempDir()

	// Save original cwd; restore after the test.
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(origDir); err != nil {
			t.Logf("cleanup chdir failed: %v", err)
		}
	})

	// Initialize a git repo in tmp.
	initCmd := exec.Command("git", "init", "-b", "main", tmp)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir(%s): %v", tmp, err)
	}

	t.Setenv("AGENTRUN_DB_DIR", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Resolve symlinks on both sides to handle macOS /var <-> /private/var.
	realTmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", tmp, err)
	}
	realDBDir, err := filepath.EvalSymlinks(cfg.DBDir)
	if err != nil {
		// DBDir may not exist yet — that's fine; resolve the parent.
		parent := filepath.Dir(cfg.DBDir)
		realParent, err2 := filepath.EvalSymlinks(parent)
		if err2 != nil {
			t.Fatalf("EvalSymlinks(%s): %v", parent, err2)
		}
		realDBDir = filepath.Join(realParent, filepath.Base(cfg.DBDir))
	}

	wantDBDir := filepath.Join(realTmp, ".agentrun")
	if realDBDir != wantDBDir {
		t.Errorf("DBDir (resolved) = %q, want %q", realDBDir, wantDBDir)
	}
}

// Test 4: Outside any git repo with HOME set, DBDir is <HOME>/.agentrun.
func TestLoad_NoRepo_HomeSet(t *testing.T) {
	// Use a temp dir that has no .git anywhere in its hierarchy.
	tmp := t.TempDir()

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(origDir); err != nil {
			t.Logf("cleanup chdir failed: %v", err)
		}
	})

	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir(%s): %v", tmp, err)
	}

	t.Setenv("HOME", "/tmp/fake_home_xyz")
	// On some Unix systems os.UserHomeDir also consults USERPROFILE; set it too
	// so the test is correct on any platform.
	t.Setenv("USERPROFILE", "/tmp/fake_home_xyz")
	t.Setenv("AGENTRUN_DB_DIR", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	want := "/tmp/fake_home_xyz/.agentrun"
	if cfg.DBDir != want {
		t.Errorf("DBDir = %q, want %q", cfg.DBDir, want)
	}
}
