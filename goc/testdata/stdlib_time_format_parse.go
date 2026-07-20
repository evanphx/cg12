package main

import "time"

func main() {
	moment := time.Date(2026, time.July, 20, 12, 34, 56, 789000000, time.UTC)
	formatted := moment.Format(time.RFC3339Nano)
	if formatted != "2026-07-20T12:34:56.789Z" {
		panic("time Format mismatch")
	}

	parsed, err := time.Parse(time.RFC3339Nano, formatted)
	if err != nil {
		panic("time Parse failed")
	}
	if !parsed.Equal(moment) {
		panic("time Parse roundtrip mismatch")
	}

	duration := 2*time.Hour + 3*time.Minute + 4*time.Second
	if duration.String() != "2h3m4s" {
		panic("duration String mismatch")
	}
}
