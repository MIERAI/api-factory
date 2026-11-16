package jobs

import (
	"testing"
	"time"
)

func TestListJobsPageTokenRoundTrip(t *testing.T) {
	want := time.Date(2026, 11, 6, 0, 0, 0, 123456000, time.UTC)
	tok := encodeListJobsPageToken(want, 7)

	got, id, err := decodeListJobsPageToken(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Equal(want) || id != 7 {
		t.Fatalf("got (%v, %d), want (%v, 7)", got, id, want)
	}
}

func TestListJobsPageTokenInvalid(t *testing.T) {
	for _, tok := range []string{
		"not-a-valid-token",
		"!!!",
		"bm8tc2VwYXJhdG9y",                 // "no-separator"
		"eHx5",                             // "x|y"
		"MjAyNi0xMS0wNlQwMDowMDowMFp8YWJj", // "2026-11-06T00:00:00Z|abc"
	} {
		if _, _, err := decodeListJobsPageToken(tok); err == nil {
			t.Errorf("decode(%q): expected error", tok)
		}
	}
}
