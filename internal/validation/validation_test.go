package validation

import "testing"

func TestClassify_TableDriven(t *testing.T) {
	cases := []struct {
		cmd      string
		wantKind Kind
		wantOK   bool
	}{
		// Tests
		{"npm test", KindTest, true},
		{"npm t", KindTest, true},
		{"yarn test", KindTest, true},
		{"pnpm test", KindTest, true},
		{"bun test", KindTest, true},
		{"npm run test:unit", KindTest, true},
		{"npm run unit:test", KindTest, true},
		{"yarn test:unit", KindTest, true},
		{"pytest", KindTest, true},
		{"pytest -k auth", KindTest, true},
		{"python -m pytest tests/", KindTest, true},
		{"python3 -m unittest discover", KindTest, true},
		{"go test ./...", KindTest, true},
		{"cargo test --release", KindTest, true},
		{"mvn test", KindTest, true},
		{"./mvnw test", KindTest, true},
		{"gradle check", KindTest, true},
		{"./gradlew test --info", KindTest, true},
		{"bundle exec rspec spec/", KindTest, true},
		{"rake test", KindTest, true},
		{"jest --watch", KindTest, true},
		{"vitest run", KindTest, true},

		// Builds
		{"npm run build", KindBuild, true},
		{"yarn build", KindBuild, true},
		{"pnpm build", KindBuild, true},
		{"go build ./...", KindBuild, true},
		{"go install ./cmd/foo", KindBuild, true},
		{"cargo build --release", KindBuild, true},
		{"mvn package", KindBuild, true},
		{"./gradlew assemble", KindBuild, true},
		{"make", KindBuild, true},
		{"make build", KindBuild, true},
		{"make all", KindBuild, true},

		// Lint
		{"npm run lint", KindLint, true},
		{"yarn lint", KindLint, true},
		{"eslint src/", KindLint, true},
		{"ruff check .", KindLint, true},
		{"flake8", KindLint, true},
		{"golangci-lint run ./...", KindLint, true},
		{"cargo clippy", KindLint, true},
		{"go vet ./...", KindLint, true},
		{"bundle exec rubocop", KindLint, true},
		{"make lint", KindLint, true},

		// Typecheck
		{"npm run typecheck", KindTypecheck, true},
		{"npm run type-check", KindTypecheck, true},
		{"yarn typecheck", KindTypecheck, true},
		{"tsc", KindTypecheck, true},
		{"tsc --noEmit", KindTypecheck, true},
		{"mypy src/", KindTypecheck, true},
		{"pyright .", KindTypecheck, true},
		{"cargo check", KindTypecheck, true},

		// cd-prefix stripping
		{"cd ./app && npm test", KindTest, true},
		{"cd /tmp && go test ./...", KindTest, true},
		{"cd app && make build", KindBuild, true},

		// Non-validation commands — must NOT match
		{"echo hello", "", false},
		{"ls -la", "", false},
		{"cat README.md", "", false},
		{"grep -r foo .", "", false},
		{"git status", "", false},
		{"docker run nginx", "", false},
		{"curl https://example.com", "", false},
		{"", "", false},
		{"npm install lodash", "", false}, // install ≠ build
		{"npm ci", "", false},
		{"yarn install", "", false},
		{"pip install requests", "", false},
		{"node script.js", "", false},
		{"python app.py", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			kind, ok := Classify(tc.cmd)
			if ok != tc.wantOK {
				t.Fatalf("Classify(%q) ok=%v, want %v (kind=%q)", tc.cmd, ok, tc.wantOK, kind)
			}
			if kind != tc.wantKind {
				t.Errorf("Classify(%q) kind=%q, want %q", tc.cmd, kind, tc.wantKind)
			}
		})
	}
}
