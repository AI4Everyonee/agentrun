package config

import (
	"os"
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

// Test 3: With HOME set and no AGENTRUN_DB_DIR, DBDir is <HOME>/.agentrun
// regardless of whether cwd is inside a git repo. (Behavior changed when the
// recorder became a global hook receiver — see config.go for rationale.)
func TestLoad_HomeSet_IgnoresGitRepo(t *testing.T) {
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
