package tempo

import (
	"testing"
	"time"
)

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func TestParseTime(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
		check   func(t *testing.T, got time.Time)
	}{
		{
			input: "now",
			check: func(t *testing.T, got time.Time) {
				if time.Since(got) > 2*time.Second {
					t.Errorf("expected time near now, got %v", got)
				}
			},
		},
		{
			input: "",
			check: func(t *testing.T, got time.Time) {
				if time.Since(got) > 2*time.Second {
					t.Errorf("empty string should resolve to now")
				}
			},
		},
		{
			input: "1h",
			check: func(t *testing.T, got time.Time) {
				want := time.Now().Add(-time.Hour)
				if absDur(want.Sub(got)) > 2*time.Second {
					t.Errorf("1h: got %v, want near %v", got, want)
				}
			},
		},
		{
			input: "30m",
			check: func(t *testing.T, got time.Time) {
				want := time.Now().Add(-30 * time.Minute)
				if absDur(want.Sub(got)) > 2*time.Second {
					t.Errorf("30m: got %v, want near %v", got, want)
				}
			},
		},
		{
			input: "2d",
			check: func(t *testing.T, got time.Time) {
				want := time.Now().Add(-48 * time.Hour)
				if absDur(want.Sub(got)) > 2*time.Second {
					t.Errorf("2d: got %v, want near %v", got, want)
				}
			},
		},
		{
			input: "2026-09-08T10:00:00Z",
			check: func(t *testing.T, got time.Time) {
				want := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
				if !got.Equal(want) {
					t.Errorf("RFC3339: got %v, want %v", got, want)
				}
			},
		},
		{input: "invalid", wantErr: true},
		{input: "1x", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseTime(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseTime(%q) error = %v, wantErr = %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestParseRelativeDuration(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
		err   bool
	}{
		{"1h", time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"45s", 45 * time.Second, false},
		{"2d", 48 * time.Hour, false},
		{"1d12h", 36 * time.Hour, false},
		{"1h30m", 90 * time.Minute, false},
		{"2d6h30m", 54*time.Hour + 30*time.Minute, false},
		{"", 0, true},
		{"bad", 0, true},
		{"1x", 0, true},
		{"h", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseRelativeDuration(tt.input)
			if (err != nil) != tt.err {
				t.Fatalf("parseRelativeDuration(%q) err = %v, wantErr = %v", tt.input, err, tt.err)
			}
			if !tt.err && got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
