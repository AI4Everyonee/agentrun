# Claude Code Hooks System - Factual Reference
**Source:** Official Claude Code documentation (code.claude.com/docs/en/hooks.md and hooks-guide.md)  
**Updated:** May 23, 2026  
**For:** Agentrun Phase 2 - Hook Layer Integration

---

## 1. Hook Event Types (Complete List)

Claude Code emits 32 hook events across three cadences:

### Session-Level Events
| Event | When It Fires | Can Block? |
|-------|---------------|-----------|
| `SessionStart` | Session begins or resumes | No |
| `Setup` | Triggered by `--init-only`, `--init`, or `--maintenance` in headless mode | No |
| `SessionEnd` | Session terminates | No |

### Turn-Level Events
| Event | When It Fires | Can Block? |
|-------|---------------|-----------|
| `UserPromptSubmit` | User submits a prompt, before Claude processes it | No |
| `UserPromptExpansion` | User-typed command expands into a prompt | Yes |
| `Stop` | Claude finishes responding | Yes |
| `StopFailure` | Turn ends due to API error | No |
| `PostToolBatch` | Batch of parallel tool calls resolves | No |
| `TeammateIdle` | Agent team teammate about to go idle | No |

### Tool-Level Events
| Event | When It Fires | Can Block? | Matcher |
|-------|---------------|-----------|---------|
| `PreToolUse` | Before tool execution | Yes | Tool name (Bash, Edit, Write, etc.) |
| `PostToolUse` | After tool succeeds | Yes | Tool name |
| `PostToolUseFailure` | After tool fails | Yes | Tool name |
| `PermissionRequest` | Permission dialog appears | Yes | Tool name |
| `PermissionDenied` | Tool denied by auto mode classifier | No | Tool name |

### Context & Configuration Events
| Event | When It Fires | Matcher |
|-------|---------------|---------|
| `InstructionsLoaded` | CLAUDE.md or `.claude/rules/*.md` loaded | Load reason (session_start, nested_traversal, etc.) |
| `ConfigChange` | Configuration file changes during session | Config source (user_settings, project_settings, etc.) |
| `CwdChanged` | Working directory changes | No matcher |
| `FileChanged` | Watched file changes on disk | Literal filenames (no regex) |
| `PreCompact` | Before context compaction | Trigger (manual, auto) |
| `PostCompact` | After context compaction | Trigger (manual, auto) |
| `WorktreeCreate` | Worktree creation via `--worktree` | No matcher |
| `WorktreeRemove` | Worktree removal | No matcher |

### Notification & Subagent Events
| Event | When It Fires | Matcher |
|-------|---------------|---------|
| `Notification` | Claude Code sends a notification | Notification type (permission_prompt, idle_prompt, auth_success, elicitation_dialog, etc.) |
| `SubagentStart` | Subagent spawned | Agent type (general-purpose, Explore, Plan, custom names) |
| `SubagentStop` | Subagent finishes | Agent type |
| `TaskCreated` | Task created via `TaskCreate` | No matcher |
| `TaskCompleted` | Task marked completed | No matcher |

### MCP & Form Events
| Event | When It Fires | Matcher |
|-------|---------------|---------|
| `Elicitation` | MCP server requests user input during tool call | MCP server name |
| `ElicitationResult` | User responds to MCP elicitation | MCP server name |

---

## 2. Hook Event Input Payload Schema

### Common Fields (All Events)
```json
{
  "session_id": "abc123...",          // Unique session ID (ULIDs or similar format)
  "cwd": "/Users/sarah/myproject",    // Working directory when event fired
  "hook_event_name": "PreToolUse",    // Event name (case-sensitive)
  "hook_from": "project_settings"     // Config source: user_settings, project_settings, local_settings, policy_settings
}
```

### PreToolUse / PostToolUse / PostToolUseFailure
```json
{
  "session_id": "abc123...",
  "cwd": "/path/to/project",
  "hook_event_name": "PreToolUse",
  "tool_name": "Bash",                // Tool being called
  "tool_input": {
    "command": "npm test"             // Tool-specific: for Bash, this is the command
  }
}
```
Tool names include: `Bash`, `Edit`, `Write`, `Read`, `Glob`, `Grep`, `WebFetch`, `WebSearch`, `NotebookEdit`, and MCP tools as `mcp__<server>__<tool>`.

### UserPromptSubmit
```json
{
  "session_id": "abc123...",
  "cwd": "/path/to/project",
  "hook_event_name": "UserPromptSubmit",
  "prompt": "Fix the bug in auth.ts"   // User's prompt text
}
```

### PermissionRequest
```json
{
  "session_id": "abc123...",
  "cwd": "/path/to/project",
  "hook_event_name": "PermissionRequest",
  "tool_name": "Bash",
  "tool_input": { "command": "..." }
}
```

### SessionStart
```json
{
  "session_id": "abc123...",
  "cwd": "/path/to/project",
  "hook_event_name": "SessionStart",
  "source": "startup|resume|clear|compact",  // How session started
  "model": "claude-sonnet-4-6",      // Current model
  "is_interactive": true
}
```

### Stop
```json
{
  "session_id": "abc123...",
  "cwd": "/path/to/project",
  "hook_event_name": "Stop",
  "stop_hook_active": false           // True if hook has already blocked 8+ times
}
```

### StopFailure
```json
{
  "session_id": "abc123...",
  "cwd": "/path/to/project",
  "hook_event_name": "StopFailure",
  "error_type": "rate_limit|authentication_failed|oauth_org_not_allowed|billing_error|invalid_request|model_not_found|server_error|max_output_tokens|unknown"
}
```

### ConfigChange
```json
{
  "session_id": "abc123...",
  "cwd": "/path/to/project",
  "hook_event_name": "ConfigChange",
  "source": "user_settings|project_settings|local_settings|policy_settings|skills",
  "file_path": "/path/to/.claude/settings.json"
}
```

### FileChanged
```json
{
  "session_id": "abc123...",
  "cwd": "/path/to/project",
  "hook_event_name": "FileChanged",
  "file_path": "/path/to/.env",       // Watched file that changed
  "relative_path": ".env"
}
```

### CwdChanged
```json
{
  "session_id": "abc123...",
  "cwd": "/new/directory",
  "hook_event_name": "CwdChanged",
  "previous_cwd": "/old/directory"
}
```

### Notification
```json
{
  "session_id": "abc123...",
  "cwd": "/path/to/project",
  "hook_event_name": "Notification",
  "notification_type": "permission_prompt|idle_prompt|auth_success|elicitation_dialog|elicitation_complete|elicitation_response"
}
```

---

## 3. Configuration Locations & Precedence

### File Paths (Highest to Lowest Priority)
1. **Managed settings** (system/MDM-deployed, cannot be overridden)
   - macOS: `com.anthropic.claudecode` plist (or `/etc/claude-code/managed-settings.json`)
   - Windows: `HKLM\SOFTWARE\Policies\ClaudeCode` or `C:\Program Files\ClaudeCode\managed-settings.json`
   - Linux: `/etc/claude-code/managed-settings.json` or `managed-settings.d/*.json`
2. **Command-line arguments** (`--settings <path>`, `--config-dir`, etc.)
3. **Local project settings** (`.claude/settings.local.json` — gitignored)
4. **Project settings** (`.claude/settings.json` — committed to repo)
5. **User settings** (`~/.claude/settings.json` — your machine only)

### Environment Variables
- `CLAUDE_CONFIG_DIR`: Override config directory (e.g., point to temp dir for testing)
- Settings in `env` block of settings.json auto-reload; most hooks reload without restart
- CLI flags like `--settings <path>` override file paths

### Merging Behavior
- When multiple sources exist, they merge with precedence as above
- A managed setting cannot be overridden by user or project settings
- Local settings fully override project settings for the same keys (not merged)
- Plugin hooks and skill/agent frontmatter hooks load independently

---

## 4. Hook Configuration JSON Schema

### Complete Hook Entry Structure
```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",                    // Optional: filter by tool/event/reason
        "if": "Bash(git *)",                  // Optional: permission rule filter (v2.1.85+)
        "hooks": [
          {
            "type": "command",                // command | http | mcp_tool | prompt | agent
            "command": "/path/to/script.sh",  // Shell command to execute
            "args": [],                       // Optional: exec form (no shell)
            "timeout": 30,                    // Optional: seconds (default varies by type)
            "statusMessage": "Validating..."  // Optional: spinner message
          }
        ]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "Edit|Write",
        "hooks": [
          {
            "type": "http",
            "url": "http://localhost:8080/hook",
            "headers": { "Authorization": "Bearer $TOKEN" },
            "allowedEnvVars": ["TOKEN"],
            "timeout": 30
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "prompt",
            "prompt": "Is this safe? $ARGUMENTS",  // Claude Haiku evaluates this
            "model": "claude-haiku-4-5"           // Optional: override model
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "agent",
            "prompt": "Verify tests pass. $ARGUMENTS",
            "timeout": 60
          }
        ]
      }
    ]
  }
}
```

### Matcher Values (Exact Match vs. Regex)
- **Exact match** (default for tool names): `Bash`, `Edit`, `Write`, `Read`
- **Pipe alternation**: `Edit|Write|Bash` matches any of those tools
- **Regex** (for anything with special chars): `^Notebook`, `mcp__.*`, `Bash|^mcp__`
- **MCP tool naming**: `mcp__<server>__<tool>` (e.g., `mcp__github__search_repositories`)
- **No matcher**: Empty string `""` or omitted → matches all occurrences
- **Literal filenames** (FileChanged only): `".envrc|.env"` (not regex, literal split by `|`)

---

## 5. Hook Execution Contract

### How the Hook is Invoked
- **Shell form** (default, if no `args`): `sh -c "command"` on macOS/Linux, Git Bash on Windows
- **Exec form** (if `args: []` present): Direct process spawn without shell (safer, faster)
- **Environment inheritance**: Hook subprocess inherits full parent environment (including `AGENTRUN_SESSION_ID` from wrapper)
- **Input delivery**: Via stdin as single JSON object (not line-delimited)
- **Output consumption**: stdout (JSON), stderr (user feedback), exit code
- **Working directory**: Same as session's `cwd` at time of event
- **Timeout**: Varies by type:
  - `command`, `http`, `mcp_tool`: 600s (10 min), lowered to 30s for `UserPromptSubmit`
  - `prompt`: 30s
  - `agent`: 60s
  - Override per hook with `timeout` field

### Exit Code Semantics
- **Exit 0**: Hook reports no objection. For `PreToolUse`, permission flow still applies (hook doesn't auto-approve).
- **Exit 2**: Block the action. Write reason to stderr; Claude receives it as feedback.
- **Any other code**: Non-blocking error. Action proceeds, transcript shows `<hook name> hook error` with first line of stderr.

### JSON Output (Exit 0 Only)
Hook can write structured JSON to stdout to influence behavior:

```json
{
  "continue": true,
  "decision": "block",                // For Stop/SubagentStop
  "reason": "Explanation",
  "suppressOutput": false,
  "additionalContext": "Key context",  // Injected into Claude's prompt
  "systemMessage": "Warning to user",
  "terminalSequence": "\033]777;notify;...\007",
  "hookSpecificOutput": {
    "hookEventName": "PreToolUse",
    "permissionDecision": "allow|deny|ask|defer",
    "permissionDecisionReason": "Why",
    "updatedInput": { "command": "modified command" },  // Rewrite tool input
    "additionalContext": "Extra context"
  }
}
```

### Read-Only Observability (No-Op Response)
For agentrun's read-only case, safest response:
```bash
exit 0  # Success, do nothing
# OR
echo '{}' && exit 0  # Empty JSON, explicit no-op
```
Both tell Claude Code: "Hook ran successfully, no decision." Action proceeds unchanged.

---

## 6. Session Correlation & Environment

### Session ID
- Delivered in every hook payload as `"session_id": "..."`
- Format: ULID or similar unique identifier (not UUID)
- Persists across resume/branch operations within the same session
- Different for each new session or `/branch`

### Environment Variable Visibility
- **Yes**: Hook subprocess inherits full environment from parent `claude` process
- `AGENTRUN_SESSION_ID` injected by your wrapper will be visible to hook scripts
- Use in hook: `$AGENTRUN_SESSION_ID` (shell form) or `os.environ['AGENTRUN_SESSION_ID']` (Python)
- Other notable inherited vars: `CLAUDE_PROJECT_DIR`, `CLAUDE_ENV_FILE`, `CLAUDE_EFFORT`, `CLAUDE_CODE_REMOTE`

### Transcript Path
- **Not in hook payload**: No `transcript_path` field delivered to hooks
- Transcripts stored at: `~/.claude/projects/<project-derived-name>/<session-id>.jsonl`
- Can override with `CLAUDE_CONFIG_DIR` env var
- **Format**: JSONL (one JSON object per line for each message, tool use, or metadata entry)
- **Retention**: Default 30 days; configurable with `cleanupPeriodDays` in settings.json
- **Stability**: Stable enough for archival; each line is a discrete event

---

## 7. Cleanup & Persistence

### State Files
- Claude Code creates **no lock files or sockets** at hook registration time
- Config is pure file-based (no IPC needed for hooks)
- Only ephemeral: temporary environment files at `$CLAUDE_ENV_FILE` (if used by CwdChanged/SessionStart hooks)

### Hook Persistence Across Restarts
- **Per-session**: Hook execution is ephemeral; each hook call is independent
- **Across restarts**: Hooks persist in settings files (`.claude/settings.json`) and are re-loaded on every session start
- **Modification safety**: Settings files can be edited while Claude Code is running; file watcher picks up changes within a few seconds
- **No cleanup required** unless you're rotating temporary config directories

### Temporary Settings Directory Merging
- If you point `CLAUDE_CONFIG_DIR` to a temp dir:
  - Claude Code **fully reads** from that dir first
  - Falls back to defaults for unspecified settings
  - **Does NOT merge** with user `~/.claude/settings.json`
  - Managed settings still apply from system location (cannot be overridden)

---

## 8. macOS-Specific Gotchas & Known Issues

### No SIP Restrictions on Hooks
- System Integrity Protection (SIP) does not restrict hook execution
- Hooks can run user-level scripts without special privileges
- Script execution works the same as manual shell commands

### Signal Handling
- Hooks run in non-interactive shells: they don't receive terminal signals
- To gracefully shutdown a long-running hook, use timeout mechanism (default 600s)
- Hook timeout is hard-enforced; exceeding it terminates the hook process

### Shell Profile Sourcing
- Non-interactive shell form (`sh -c`) may source `~/.bashrc` or `~/.zshrc` if `BASH_ENV` is set
- **Problem**: Unconditional `echo` statements in profile prepend output before JSON, causing parse failure
- **Fix**: Wrap profile echo statements in `if [[ $- == *i* ]]; then ... fi` (interactive-only)

### Git Bash on Windows (via WSL or Git for Windows)
- Default shell form on Windows uses Git Bash
- Same JSON validation issue as above if profile emits output
- Use `args: []` (exec form) to bypass shell entirely and avoid profile sourcing

---

## 9. Error Handling & Debugging

### Non-Zero Exit Codes
- **Exit 2**: Blocks action (for blockable events); stderr shown to user
- **Other codes (1, 3, ...)**:
  - Non-blocking error
  - Execution continues
  - Transcript shows `<hook name> hook error: <first line of stderr>`
  - Full stderr goes to debug log (visible with `/debug` or `--debug-file`)

### Command Not Found
- If script is not on `$PATH`, hook fails with "command not found"
- **Fix**: Use absolute path or `${CLAUDE_PROJECT_DIR}/.claude/hooks/script.sh`
- Or switch to exec form: `"args": []` + `"command": "/absolute/path/to/binary"`

### Timeout Behavior
- Hook exceeds timeout → process is killed
- Transcript shows timeout error in debug log
- Action may proceed or block depending on event type

### Logging & Debug Output
- **Interactive**: `Ctrl+O` toggles transcript view (shows one-line summary per hook)
- **Debug log**: Start with `claude --debug-file /tmp/claude.log` or run `/debug` mid-session
  - Log shows: which hooks matched, exit codes, full stdout/stderr, timing
- **Hook output to stderr**: Goes to debug log (doesn't pollute stdout JSON)

### View All Configured Hooks
- Run `/hooks` in Claude Code
- Shows every event with hook count
- Select an event to see matcher, type, source file, command
- Read-only view (edit by modifying settings.json directly)

---

## 10. Disabling & Control

### Disable All Hooks
```json
{
  "disableAllHooks": true
}
```
Set in any settings file. Managed settings `disableAllHooks` still takes precedence.

### Matcher Case Sensitivity
- Tool name matchers are **case-sensitive**: `Bash` ≠ `bash`
- Regex matchers also case-sensitive unless you use `(?i)`

### Combining Multiple Hooks
- When multiple hooks match the same event, all run in **parallel**
- Results are merged: most restrictive wins (deny > ask > allow for permission events)
- One hook's failure does not block sibling hooks from executing
- Order is non-deterministic if multiple hooks modify same input (avoid this)

---

## 11. Async & Advanced Hook Types

### HTTP Hooks
- POST payload to `url` instead of spawning process
- Response body uses same JSON format as command hook output
- `headers` can interpolate env vars: `"$MY_TOKEN"` or `"${MY_TOKEN}"` (must be in `allowedEnvVars`)
- HTTP status: only 2xx + JSON response blocks; status code alone cannot block

### MCP Tool Hooks
- Call an already-connected MCP server tool directly
- Requires `"server": "mcp_server_name"` and `"tool": "tool_name"`
- Input/output same JSON format
- Useful for integrating hooks with external systems via MCP

### Prompt-Based Hooks
- Single-turn LLM call (Claude Haiku by default)
- Model responds with `{"ok": true}` or `{"ok": false, "reason": "..."}`
- Timeout: 30s
- Event-specific behavior if `"ok": false`

### Agent-Based Hooks
- Multi-turn subagent with tool access (file read, code search, etc.)
- Response format: `{"ok": true/false, "reason": "..."}`
- Timeout: 60s default
- **Experimental**: behavior may change in future releases

---

## 12. Critical Gotchas for Agentrun

1. **Session ID not on every event**: Some events (e.g., `Notification`) deliver `session_id`, but verify in your event handler
2. **No transcript in payload**: Must read transcript JSONL file from disk if you need full history
3. **Merging behavior for local settings**: `CLAUDE_CONFIG_DIR` doesn't merge with `~/.claude/settings.json`; use it for complete isolation
4. **Stop hook block cap**: Stop hooks that block >8 times in a row are overridden; check `stop_hook_active` flag
5. **Permission rules override hook approvals**: `"allow"` from hook doesn't bypass deny rules; deny rules always win
6. **Parallel hook execution**: Avoid two hooks modifying the same tool input; last to finish wins (non-deterministic)
7. **PermissionRequest in headless mode**: Doesn't fire in `-p` (non-interactive); use `PreToolUse` instead
8. **Exit code 2 blocks blockable events only**: For non-blockable events, exit 2 shows stderr to user but execution continues
9. **Empty matcher** ("") is not the same as **omitted matcher**: Both work, but be consistent

---

## Summary Table: Events, Input Fields, Block Capability

| Event | Input Fields | Can Block? | Matcher Type |
|-------|--------------|-----------|--------------|
| SessionStart | source, model, is_interactive | No | Startup reason (startup/resume/clear/compact) |
| PreToolUse | tool_name, tool_input | Yes | Tool name |
| PostToolUse | tool_name, tool_input | Yes | Tool name |
| Stop | stop_hook_active | Yes | None (no matcher) |
| PermissionRequest | tool_name, tool_input | Yes | Tool name |
| UserPromptSubmit | prompt | No | None |
| ConfigChange | source, file_path | Yes (exit 2) | Config source |
| FileChanged | file_path, relative_path | No | Literal filenames |
| CwdChanged | cwd, previous_cwd | No | None |
| SessionEnd | None documented | No | Termination reason |

---

## References

- **Official docs**: https://code.claude.com/docs/en/hooks.md
- **Guide**: https://code.claude.com/docs/en/hooks-guide.md
- **Settings**: https://code.claude.com/docs/en/settings.md
- **Sessions**: https://code.claude.com/docs/en/sessions.md
- **Environment variables**: https://code.claude.com/docs/en/env-vars.md
