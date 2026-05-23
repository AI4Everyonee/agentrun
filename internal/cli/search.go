package cli

// runSearch implements `agentrun search <query> [--limit N] [--session <id>] [--type <type>]`
//
// Full-text search over event payloads using the FTS5 virtual table events_fts.
//
// FTS5 query syntax quick reference:
//   - Quoted phrase:   "rate limit"
//   - Boolean:         error AND timeout
//   - Prefix:          tool_*   (matches tool_name, tool_input, etc.)
//   - Column scope:    payload : timeout  (only match in payload column)
//   - Negation:        error NOT timeout
//
// Output format (two lines per hit):
//
//	<session_id (30 char)>  <agent (8 char)>  <type>  <ts>
//	  <snippet around the matched term>

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"unicode"

	"golang.org/x/term"

	"github.com/jeevan/agentrun/internal/db"
)

func runSearch(args []string) error {
	// Separate the first non-flag argument (the query) from the flag args.
	// This allows flags to appear either before or after the query.
	query, flagArgs, err := extractQuery(args)
	if err != nil || query == "" {
		return ErrUsage
	}

	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "max results to return")
	sessionID := fs.String("session", "", "restrict to this session ID")
	typeFilter := fs.String("type", "", "restrict to events whose type contains this substring")

	if err := fs.Parse(flagArgs); err != nil {
		return ErrUsage
	}

	dbPath, err := resolveDBPath()
	if err != nil {
		return err
	}

	d, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	hits, err := db.SearchEvents(d, query, *sessionID, *typeFilter, *limit)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}

	if len(hits) == 0 {
		fmt.Printf("agentrun: no matches for %q\n", query)
		return nil
	}

	isTTY := term.IsTerminal(int(os.Stdout.Fd()))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, h := range hits {
		sid := truncate(h.SessionID, 30)
		agent := truncate(h.Agent, 8)
		ts := h.Ts.Local().Format("2006-01-02 15:04:05")

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", sid, agent, h.Type, ts)

		snip := extractSnippet(h.Snippet, query, 120)
		if isTTY {
			// Substitute << and >> markers with ANSI bold.
			snip = boldMarkers(snip)
		}
		fmt.Fprintf(w, "  %s\n\n", snip)
	}
	return w.Flush()
}

// extractQuery separates the first non-flag argument (the search query) from
// the remaining flag arguments. It understands --flag and --flag=value forms.
// Returns ("", nil, nil) when no positional arg is found.
func extractQuery(args []string) (query string, flagArgs []string, err error) {
	i := 0
	for i < len(args) {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			// This is a flag. Check if it takes a value (--flag value form).
			flagArgs = append(flagArgs, a)
			// If it's not --flag=value form and has a known value-taking flag name,
			// also consume the next arg as the flag value.
			if !strings.Contains(a, "=") {
				stripped := strings.TrimLeft(a, "-")
				switch stripped {
				case "limit", "session", "type":
					if i+1 < len(args) {
						i++
						flagArgs = append(flagArgs, args[i])
					}
				}
			}
		} else if query == "" {
			// First non-flag arg is the query.
			query = a
		}
		i++
	}
	return query, flagArgs, nil
}

// extractSnippet finds the first occurrence of any word from query in text and
// returns a window of up to windowSize runes centred around it, wrapping the
// matched term with << and >>. Falls back to the first windowSize chars when
// no term is found.
func extractSnippet(text, query string, windowSize int) string {
	// Strip FTS5 operators and quoted-phrase delimiters from the query to get
	// plain search terms.
	terms := extractTerms(query)

	lower := strings.ToLower(text)
	bestIdx := -1
	bestLen := 0
	for _, term := range terms {
		t := strings.ToLower(term)
		if idx := strings.Index(lower, t); idx >= 0 && (bestIdx < 0 || idx < bestIdx) {
			bestIdx = idx
			bestLen = len(t)
		}
	}

	runes := []rune(text)
	total := len(runes)

	if bestIdx < 0 || total == 0 {
		// No match found in text; return a plain prefix.
		if total <= windowSize {
			return text
		}
		return string(runes[:windowSize]) + "…"
	}

	// Convert byte offset to rune offset.
	runeIdx := len([]rune(text[:bestIdx]))
	matchRuneLen := len([]rune(text[bestIdx : bestIdx+bestLen]))

	half := windowSize / 2
	start := runeIdx - half
	if start < 0 {
		start = 0
	}
	end := start + windowSize
	if end > total {
		end = total
		start = end - windowSize
		if start < 0 {
			start = 0
		}
	}

	var sb strings.Builder
	if start > 0 {
		sb.WriteString("…")
	}
	sb.WriteString(string(runes[start:runeIdx]))
	sb.WriteString("<<")
	sb.WriteString(string(runes[runeIdx : runeIdx+matchRuneLen]))
	sb.WriteString(">>")
	afterMatch := runeIdx + matchRuneLen
	if afterMatch < end {
		sb.WriteString(string(runes[afterMatch:end]))
	}
	if end < total {
		sb.WriteString("…")
	}
	return sb.String()
}

// extractTerms parses a FTS5 query string into individual search terms,
// stripping boolean operators (AND, OR, NOT) and FTS5 syntax characters.
func extractTerms(query string) []string {
	// Remove FTS5 column scope (e.g., "payload : foo" → "foo").
	if idx := strings.Index(query, ":"); idx >= 0 {
		query = query[idx+1:]
	}

	var terms []string
	// Split on whitespace and FTS5 syntax characters.
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return unicode.IsSpace(r) || r == '"' || r == '(' || r == ')' || r == '*'
	})
	skip := map[string]bool{"AND": true, "OR": true, "NOT": true}
	for _, f := range fields {
		if f == "" || skip[f] {
			continue
		}
		// Strip trailing FTS5 prefix wildcard from the term itself.
		f = strings.TrimRight(f, "*")
		if f != "" {
			terms = append(terms, f)
		}
	}
	return terms
}

// boldMarkers replaces the << and >> snippet markers with ANSI bold escape
// sequences for TTY output.
func boldMarkers(s string) string {
	out := make([]byte, 0, len(s)+32)
	i := 0
	for i < len(s) {
		if i+2 <= len(s) && s[i] == '<' && s[i+1] == '<' {
			out = append(out, "\033[1m"...)
			i += 2
			continue
		}
		if i+2 <= len(s) && s[i] == '>' && s[i+1] == '>' {
			out = append(out, "\033[0m"...)
			i += 2
			continue
		}
		out = append(out, s[i])
		i++
	}
	return string(out)
}
