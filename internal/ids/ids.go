package ids

import (
	"crypto/rand"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

var (
	entropyMu sync.Mutex
	entropy   = ulid.Monotonic(rand.Reader, 0)
)

// New returns a fresh 26-character ULID string with no prefix.
func New() string {
	entropyMu.Lock()
	defer entropyMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
}

// Session returns "s_<ulid>".
func Session() string { return "s_" + New() }

// Event returns "evt_<ulid>".
func Event() string { return "evt_" + New() }

// Artifact returns "art_<ulid>".
func Artifact() string { return "art_" + New() }
