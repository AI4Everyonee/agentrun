// Package validation classifies shell commands as test/build/lint/typecheck so
// the recorder can roll them up into session_summary's validations_* counters.
//
// Heuristics are deliberately conservative — we'd rather miss a real validation
// command than count a noisy command (e.g. plain `echo`) as one. Match on the
// leftmost command after stripping any `cd … && ` prefix.
package validation

import (
	"strings"
)

// Kind enumerates the validation categories we recognize.
type Kind string

const (
	KindTest      Kind = "test"
	KindBuild     Kind = "build"
	KindLint      Kind = "lint"
	KindTypecheck Kind = "typecheck"
)

// Classify inspects a raw shell command string and reports whether it looks
// like a validation command. ok=false means no match — caller should ignore
// the kind value in that case.
//
// Examples:
//
//	Classify("npm test")          → (KindTest, true)
//	Classify("cd app && pytest")  → (KindTest, true)
//	Classify("ls -la")            → ("", false)
func Classify(cmd string) (Kind, bool) {
	tokens := tokenize(stripCDPrefix(cmd))
	if len(tokens) == 0 {
		return "", false
	}
	return classifyTokens(tokens)
}

// stripCDPrefix removes a leading `cd <path> &&` so the classification rules
// don't need to know about that idiom. Conservative: only strips ONE level.
func stripCDPrefix(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "cd ") {
		return s
	}
	idx := strings.Index(s, "&&")
	if idx < 0 {
		return s
	}
	return strings.TrimSpace(s[idx+2:])
}

// tokenize splits cmd on whitespace, dropping empties. Doesn't handle shell
// quoting — for the patterns we recognize this is fine.
func tokenize(s string) []string {
	return strings.Fields(s)
}

// classifyTokens matches the token sequence against known validation idioms.
// Order matters when the same first token can mean different kinds (e.g.
// `cargo test` vs `cargo build` vs `cargo clippy` vs `cargo check`).
func classifyTokens(tokens []string) (Kind, bool) {
	if len(tokens) == 0 {
		return "", false
	}
	first := tokens[0]

	// npm/yarn/pnpm/bun aliases: dispatch on second token.
	switch first {
	case "npm", "pnpm", "yarn", "bun":
		return classifyJSRunner(tokens)
	}

	// `python -m <module>` and `python3 -m <module>`.
	if (first == "python" || first == "python3") && len(tokens) >= 3 && tokens[1] == "-m" {
		switch tokens[2] {
		case "pytest", "unittest", "nose2":
			return KindTest, true
		case "mypy", "pyright":
			return KindTypecheck, true
		case "ruff", "flake8", "pylint":
			return KindLint, true
		}
	}

	// Single-binary commands.
	switch first {
	// Test runners
	case "pytest", "jest", "vitest", "mocha", "ava", "tap", "rspec", "rake", "phpunit":
		return KindTest, true

	// Lint / type-check that aren't ambiguous with build
	case "eslint", "prettier", "ruff", "flake8", "pylint", "rubocop", "stylelint", "golangci-lint":
		return KindLint, true
	case "tsc", "mypy", "pyright", "flow":
		return KindTypecheck, true

	// `make` defaults to build unless it has a "test"/"lint" target.
	case "make":
		return classifyMake(tokens)
	}

	// go subcommands.
	if first == "go" && len(tokens) >= 2 {
		switch tokens[1] {
		case "test":
			return KindTest, true
		case "build", "install":
			return KindBuild, true
		case "vet":
			return KindLint, true
		}
	}

	// cargo subcommands.
	if first == "cargo" && len(tokens) >= 2 {
		switch tokens[1] {
		case "test":
			return KindTest, true
		case "build", "install":
			return KindBuild, true
		case "clippy":
			return KindLint, true
		case "check":
			return KindTypecheck, true
		}
	}

	// mvn / gradle.
	if first == "mvn" || first == "./mvnw" {
		if hasGoal(tokens, "test") {
			return KindTest, true
		}
		if hasGoal(tokens, "package", "install", "compile") {
			return KindBuild, true
		}
	}
	if first == "gradle" || first == "./gradlew" {
		if hasGoal(tokens, "test", "check") {
			return KindTest, true
		}
		if hasGoal(tokens, "build", "assemble", "compileJava") {
			return KindBuild, true
		}
	}

	// `bundle exec rspec` etc.
	if first == "bundle" && len(tokens) >= 3 && tokens[1] == "exec" {
		switch tokens[2] {
		case "rspec":
			return KindTest, true
		case "rubocop":
			return KindLint, true
		}
	}

	return "", false
}

// classifyJSRunner handles `npm/yarn/pnpm/bun ...` patterns.
//
// Direct shortcuts: `npm test`, `yarn test`, `pnpm test`, `bun test`.
// Run-script forms: `npm run <script>`, `yarn <script>`, `pnpm run <script>`.
func classifyJSRunner(tokens []string) (Kind, bool) {
	if len(tokens) < 2 {
		return "", false
	}
	script := tokens[1]

	// `<runner> test` / `<runner> build` shortcuts.
	switch script {
	case "test", "t":
		return KindTest, true
	case "build":
		return KindBuild, true
	case "lint":
		return KindLint, true
	case "typecheck", "type-check", "tsc":
		return KindTypecheck, true
	}

	// `<runner> run <script>` or `<runner> exec <script>`.
	if (script == "run" || script == "exec" || script == "x") && len(tokens) >= 3 {
		k := classifyScriptName(tokens[2])
		return k, k != ""
	}

	// yarn shorthand: `yarn <script>` (yarn doesn't require `run`).
	if tokens[0] == "yarn" {
		k := classifyScriptName(script)
		return k, k != ""
	}

	return "", false
}

// classifyScriptName guesses a package.json script name's category by
// matching common suffixes/prefixes.
func classifyScriptName(name string) Kind {
	n := strings.ToLower(name)
	switch {
	case n == "test" || strings.HasPrefix(n, "test:") || strings.HasSuffix(n, ":test"):
		return KindTest
	case n == "build" || strings.HasPrefix(n, "build:") || strings.HasSuffix(n, ":build"):
		return KindBuild
	case n == "lint" || strings.Contains(n, "eslint") || strings.HasPrefix(n, "lint:"):
		return KindLint
	case n == "typecheck" || n == "type-check" || n == "tsc" || strings.Contains(n, "typecheck"):
		return KindTypecheck
	}
	return ""
}

// classifyMake inspects make's targets.
func classifyMake(tokens []string) (Kind, bool) {
	if len(tokens) == 1 {
		return KindBuild, true // `make` alone builds the default target
	}
	for _, t := range tokens[1:] {
		if strings.HasPrefix(t, "-") {
			continue
		}
		switch t {
		case "test", "tests", "check":
			return KindTest, true
		case "lint":
			return KindLint, true
		case "build", "all", "compile":
			return KindBuild, true
		case "typecheck", "type-check":
			return KindTypecheck, true
		}
	}
	return KindBuild, true
}

// hasGoal reports whether any of wants appears as a non-flag token after the
// first (used for mvn/gradle goal detection).
func hasGoal(tokens []string, wants ...string) bool {
	for _, t := range tokens[1:] {
		if strings.HasPrefix(t, "-") {
			continue
		}
		for _, w := range wants {
			if t == w {
				return true
			}
		}
	}
	return false
}
