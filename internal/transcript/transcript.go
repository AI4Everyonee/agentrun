// Package transcript copies the agent's own session JSONL (the "transcript")
// into the agentrun artifacts directory, gzipped. This gives us the one
// signal hook events don't carry: the assistant's natural-language replies.
//
// Both Claude and Codex write their own JSONL transcripts:
//   - Claude: ~/.claude/projects/<derived>/<uuid>.jsonl
//   - Codex:  ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl
//
// The path is captured into sessions.transcript_path by our hooks. At session
// end we copy the file to <artifacts>/<sid>/transcript.jsonl.gz and record an
// artifact row.
//
// Limitations:
//   - Files larger than maxTranscriptBytes are skipped with a stderr warning
//     (default 50 MB raw; override via $AGENTRUN_MAX_TRANSCRIPT_MB).
//   - If the same session ends multiple times (e.g. resumed after sweep),
//     each call REPLACES the previous artifact rather than appending. This is
//     the simplest semantic — the latest transcript is the most complete one.
package transcript

import (
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/AI4Everyonee/agentrun/internal/db"
	"github.com/AI4Everyonee/agentrun/internal/ids"
)

const (
	defaultMaxMB       = 50
	artifactKind       = "transcript"
	artifactMimeGzip   = "application/gzip"
	artifactFilename   = "transcript.jsonl.gz"
	envMaxTranscriptMB = "AGENTRUN_MAX_TRANSCRIPT_MB"
)

// Capture reads the agent's transcript JSONL at transcriptPath, gzips it to
// <artifactsDir>/<sessionID>/transcript.jsonl.gz, and inserts an artifacts
// row. Best-effort: any failure is returned as an error but callers should
// log-and-continue (the session is otherwise complete).
//
// If transcriptPath is empty, the source file doesn't exist, or the raw size
// exceeds the cap, Capture returns nil without touching the DB.
func Capture(d *sql.DB, sessionID, transcriptPath, artifactsDir string) error {
	if transcriptPath == "" {
		return nil
	}
	src, err := os.Open(transcriptPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Common when Claude rotated/deleted an old transcript. Not an error.
			return nil
		}
		return fmt.Errorf("transcript.Capture: open %q: %w", transcriptPath, err)
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("transcript.Capture: stat %q: %w", transcriptPath, err)
	}
	maxBytes := readMaxBytes()
	if info.Size() > maxBytes {
		fmt.Fprintf(os.Stderr,
			"agentrun: transcript skipped — %s is %d MB (cap %d MB; set %s to override)\n",
			transcriptPath, info.Size()/(1024*1024), maxBytes/(1024*1024), envMaxTranscriptMB,
		)
		return nil
	}

	dstDir := filepath.Join(artifactsDir, sessionID)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("transcript.Capture: mkdir %q: %w", dstDir, err)
	}
	dstPath := filepath.Join(dstDir, artifactFilename)

	dst, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("transcript.Capture: create %q: %w", dstPath, err)
	}
	defer dst.Close()

	gz := gzip.NewWriter(dst)
	hasher := sha256.New()
	multi := io.MultiWriter(gz, hasher)

	rawSize, err := io.Copy(multi, src)
	if err != nil {
		return fmt.Errorf("transcript.Capture: copy: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("transcript.Capture: gzip close: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("transcript.Capture: file close: %w", err)
	}

	compInfo, err := os.Stat(dstPath)
	if err != nil {
		return fmt.Errorf("transcript.Capture: stat compressed: %w", err)
	}

	// Replace any prior transcript artifact for this session. We INSERT a new
	// row (with a fresh ULID) and rely on consumers to pick the most recent.
	// A "delete previous" pass is also fine but adds another transaction;
	// callers can do it themselves if they care.
	row := db.ArtifactRow{
		ID:          ids.Artifact(),
		SessionID:   sessionID,
		Kind:        artifactKind,
		Path:        sql.NullString{String: dstPath, Valid: true},
		ContentHash: sql.NullString{String: hex.EncodeToString(hasher.Sum(nil)), Valid: true},
		SizeBytes:   sql.NullInt64{Int64: compInfo.Size(), Valid: true},
		Mime:        sql.NullString{String: artifactMimeGzip, Valid: true},
		MetadataJSON: fmt.Sprintf(
			`{"source_path":%q,"uncompressed_size":%d,"compressed_size":%d}`,
			transcriptPath, rawSize, compInfo.Size(),
		),
	}
	if err := db.InsertArtifact(d, row); err != nil {
		return fmt.Errorf("transcript.Capture: insert artifact: %w", err)
	}
	return nil
}

// readMaxBytes returns the per-transcript cap in bytes. Honors
// $AGENTRUN_MAX_TRANSCRIPT_MB; default defaultMaxMB.
func readMaxBytes() int64 {
	if v := os.Getenv(envMaxTranscriptMB); v != "" {
		if mb, err := strconv.Atoi(v); err == nil && mb > 0 {
			return int64(mb) * 1024 * 1024
		}
	}
	return int64(defaultMaxMB) * 1024 * 1024
}
