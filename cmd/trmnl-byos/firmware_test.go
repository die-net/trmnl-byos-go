package main

import (
	"testing"
	"time"
)

func TestOffered(t *testing.T) {
	t.Parallel()
	if got := offered(0); got != "-" {
		t.Errorf("offered(0) = %q, want -", got)
	}
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.Local)
	if got, want := offered(at.Unix()), "2026-03-04 05:06:07"; got != want {
		t.Errorf("offered = %q, want %q", got, want)
	}
}
