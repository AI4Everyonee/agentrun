package ids

import (
	"strings"
	"sync"
	"testing"
)

func TestPrefixes(t *testing.T) {
	if !strings.HasPrefix(Session(), "s_") {
		t.Errorf("Session() missing s_ prefix")
	}
	if !strings.HasPrefix(Event(), "evt_") {
		t.Errorf("Event() missing evt_ prefix")
	}
	if !strings.HasPrefix(Artifact(), "art_") {
		t.Errorf("Artifact() missing art_ prefix")
	}
}

func TestUnique(t *testing.T) {
	const goroutines = 100
	const perG = 100
	out := make(chan string, goroutines*perG)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				out <- New()
			}
		}()
	}
	wg.Wait()
	close(out)

	seen := make(map[string]struct{}, goroutines*perG)
	for id := range out {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id: %s", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != goroutines*perG {
		t.Fatalf("expected %d ids, got %d", goroutines*perG, len(seen))
	}
}

func TestLength(t *testing.T) {
	if got := len(New()); got != 26 {
		t.Errorf("New() length = %d, want 26", got)
	}
}
