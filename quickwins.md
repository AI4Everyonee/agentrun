  Quick wins (1–2 hour items, immediate value)
  
  1. finalize-idle as a cron / launchd job — drop a plist that runs it every 30 min so Codex sessions auto-close. No code change, just a config snippet to ship in examples/.
  2. Normalize subagent events — add SubagentStart/SubagentStop/TaskCreated/TaskCompleted to internal/hooks/normalize.go. Currently they land as hook.SubagentStart (catch-all). Tool-call-like events would
   map to tool.* types.
  3. Capture Claude's model from the transcript JSONL — we already store transcript_path. On SessionEnd, read the first line, extract model, fill sessions.model. ~30 lines.
  4. agentrun gc --older-than 30d — delete old sessions + their artifacts. The DB grows forever today.
  5. agentrun watch — tail events in real time across all sessions (think tail -f for hook activity). Small but very useful while debugging.
  6. agentrun doctor — sanity check: claude/codex on PATH, hooks installed correctly, DB writable, last session less than N hours ago. Small.
  7. Statusline / shell prompt integration — env var the wrapper exports so $AGENTRUN_SESSION_ID shows in zsh prompt. Trivial.
  8. Install race protection — flock on the settings files during install/uninstall.

  Medium-effort, real new capability
  
  9. PTY redaction — decode the base64 chunker output, redact, re-encode. Closes the biggest data-leak gap. Some perf overhead but worth it.
  10. Token/cost capture — regex-parse the "tokens used" line from PTY output (both agents emit different formats). Bumps a tokens_used/cost_usd_cents column. Wrapper-only.
  11. Better validation pass/fail signal — parse common test runner output (X passed / Y failed) from PostToolUse tool_response.stdout rather than relying on exit code.
  12. Full-text search across sessions — SQLite FTS5 over events.payload_json and user prompts. agentrun search "regex pattern" returns hits with context.
  13. Better agentrun show — group by Claude's transcript_path UUID to thread events across resumed sessions; show validation breakdown; show diff stat inline.
  14. Per-project rollup — agentrun stats [--repo /path] showing usage per repo: tool calls, validations, error rate, hours active.
  15. Session diff — agentrun diff <session_id> opens the captured git_diff artifact in $PAGER (delta/bat aware).
  16. HTTP collector mode — long-lived agentrun process accepting hook events via Unix socket instead of fork-per-hook. Cuts hook latency from ~10ms to ~1ms. Useful only if you're firing many hooks per
  second.
  17. Session replay — for wrapped sessions, agentrun replay <id> cats the raw PTY back through your terminal at original speed (like asciinema play).
  18. Comparison runs — agentrun compare s_a s_b shows side-by-side event timelines (good for "Claude vs Codex on the same task").

  Bigger investments  

  19. Web UI for inspection — local-only HTTP server, browse sessions, drill into events, view diffs, replay PTY. The single most impactful UX upgrade. ~1–2 days.
  20. Definition-of-Done enforcement (GOAL.md §15) — extend hooks from read-only to policy: block session completion if tests fail, auto-prompt agent to fix. Philosophical shift — recorder becomes
  enforcer.
  21. Failure-loop detection — flag when the same tool fails 3+ times in a row, alert via notification.
  22. Auto-summary at session end — fire one cheap Haiku/gpt-5-nano call summarizing what happened, store as session.summary text. Tiny cost, big inspection win.
  23. Multi-machine sync — upload sessions to a team server (S3/object store + sync metadata). Real Phase 8 work.
  24. OpenTelemetry / Honeycomb / Sentry export — emit events to a real observability backend so you get charts, alerts, SLOs over agent activity.
  25. VS Code / Cursor extension — open a session inside the editor, navigate the diff with file-by-file scrub.
  26. MCP server for agentrun — agentrun-mcp exposes the session DB to other agents. Let Claude query "what did I do last week in this repo?" via MCP.

  Cross-cutting / polish 

  27. Schema migrations — proper versioning so we can ALTER TABLE in future releases without users losing data.
  28. Compressed artifacts — gzip pty.raw and git.diff (typically 10:1 ratio). Saves disk.
  29. Per-event tags — add a tags column for user-defined annotations (e.g., mark a session "production-deploy" or "experimental").
  30. Telemetry honesty — log when hook execution itself takes >50ms or when payloads are truncated, so users can spot when their recordings are incomplete.

  ---
  My suggestion on priority

  If I had to pick six, in this order:

  1. finalize-idle cron + agentrun doctor — make the install resilient and self-sustaining (~30 min)
  2. agentrun gc + schema migrations — keep the DB healthy long-term (~1 hr)
  3. Web UI — single biggest UX leap, makes everything else discoverable (~1 day)
  4. PTY redaction + token/cost capture — close the most-asked-about gaps (~2 hr)
  5. Auto-summary at session end — turns a pile of events into a one-line readable answer to "what did the agent do?" (~1 hr)
  6. Full-text search — once the DB has weeks of data, search becomes the killer feature (~2 hr)

  Want me to start on a specific subset, or are you OK leaving this where it is and trying it out for a while first?
