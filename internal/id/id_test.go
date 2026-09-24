package id

import (
	"strings"
	"testing"
	"time"
)

func TestNewIsUniqueAndPrefixed(t *testing.T) {
	seen := make(map[string]bool, 10000)
	for range 10000 {
		got := New("org")
		if !strings.HasPrefix(got, "org_") {
			t.Fatalf("New(\"org\") = %q", got)
		}
		if !HasPrefix(got, "org") {
			t.Fatalf("HasPrefix rejects an id it just minted: %q", got)
		}
		if seen[got] {
			t.Fatalf("New returned %q twice", got)
		}
		seen[got] = true
	}
}

func TestNewSortsByCreationTime(t *testing.T) {
	// Sortable ids are what make "ORDER BY id" a chronological listing.
	first := New("key")
	time.Sleep(2 * time.Millisecond)
	second := New("key")
	if first >= second {
		t.Errorf("%q was minted before %q but does not sort before it", first, second)
	}
}

func TestHasPrefixRejectsOtherShapes(t *testing.T) {
	if HasPrefix("org_short", "org") {
		t.Error("a truncated id was accepted")
	}
	if HasPrefix(New("team"), "org") {
		t.Error("an id with a different prefix was accepted")
	}
}
