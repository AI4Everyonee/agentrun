// Package tokenparse scrapes "tokens used: N" / "N input tokens, M output tokens"
// lines out of agent terminal output. Each agent prints this differently:
//
//   - Codex prints `tokens used\n11,728` (newline-separated, comma thousands)
//   - Claude prints various phrasings depending on version (`Tokens: N`, etc.)
//
// We try the patterns most-recent-first and return the LAST match. The latest
// number in a session is the cumulative final count.
//
// Stripping ANSI before parsing is essential — both agents draw colored
// terminal UIs that interleave escape sequences between visible characters.
package tokenparse

import (
	"regexp"
	"strconv"
	"strings"
)

// Result is the extracted rollup. Cost is in USD cents; 0 means not reported.
type Result struct {
	Tokens    int
	CostCents int
}

// Extract scans text for token / cost patterns and returns the last match found.
// ok=false means nothing recognized — caller should leave the columns NULL.
//
// `text` should ALREADY have ANSI escape codes stripped — see StripANSI.
func Extract(text string) (Result, bool) {
	var out Result
	var found bool

	for _, p := range tokenPatterns {
		for _, m := range p.re.FindAllStringSubmatch(text, -1) {
			n, err := strconv.Atoi(strings.ReplaceAll(m[p.group], ",", ""))
			if err != nil {
				continue
			}
			out.Tokens = n
			found = true
		}
	}

	for _, p := range costPatterns {
		for _, m := range p.re.FindAllStringSubmatch(text, -1) {
			f, err := strconv.ParseFloat(m[p.group], 64)
			if err != nil {
				continue
			}
			// Patterns express dollars; persist as cents.
			out.CostCents = int(f * 100)
			found = true
		}
	}

	return out, found
}

// StripANSI removes the common terminal control sequences (CSI, OSC, simple
// SGR) so the patterns above can match cleanly. Conservative: only strips
// sequences known to wrap printable content, not raw bytes outside 0x00–0x7f.
func StripANSI(s string) string {
	s = ansiCSI.ReplaceAllString(s, "")
	s = ansiOSC.ReplaceAllString(s, "")
	return s
}

var (
	// CSI: ESC [ ... letter (e.g. ESC[31m for color)
	ansiCSI = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	// OSC: ESC ] ... BEL or ESC \  (e.g. iTerm title strings)
	ansiOSC = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
)

type pattern struct {
	re    *regexp.Regexp
	group int
}

// tokenPatterns match anywhere in the stripped text. `group` is the index of
// the capture group holding the integer string (which may contain commas).
var tokenPatterns = []pattern{
	// Codex prints  `tokens used\n11,728`  (label on previous line)
	{regexp.MustCompile(`(?i)tokens\s+used[\s:]*([0-9][\d,]*)`), 1},
	// Claude historical formats:
	{regexp.MustCompile(`(?i)\btokens?\s*[:=]\s*([0-9][\d,]*)`), 1},
	{regexp.MustCompile(`(?i)([0-9][\d,]*)\s+tokens?\b`), 1},
	// Generic "Total tokens: N"
	{regexp.MustCompile(`(?i)total\s+tokens?[\s:]*([0-9][\d,]*)`), 1},
}

// costPatterns recognize various dollar-cost formats. Currently we only see
// these in extended Claude output; Codex doesn't seem to print cost.
var costPatterns = []pattern{
	{regexp.MustCompile(`(?i)cost[:=]\s*\$([0-9]+\.[0-9]+)`), 1},
	{regexp.MustCompile(`(?i)\$([0-9]+\.[0-9]+)\s+(?:total|usd|so far)`), 1},
}
