package main

import (
	"testing"
	"time"
)

func TestNormalizeTempWorkers(t *testing.T) {
	cases := []struct {
		name          string
		requested     int
		allowParallel bool
		want          int
	}{
		{name: "off forces single", requested: 8, allowParallel: false, want: 1},
		{name: "on keeps valid", requested: 4, allowParallel: true, want: 4},
		{name: "on clamps low", requested: 0, allowParallel: true, want: 1},
		{name: "on clamps high", requested: 99, allowParallel: true, want: 50},
	}

	for _, tc := range cases {
		got := normalizeTempWorkers(tc.requested, tc.allowParallel)
		if got != tc.want {
			t.Fatalf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

func TestTempMailPostSuccessDelaySeconds(t *testing.T) {
	delay0 := 0
	delay12 := 12
	delayHigh := 999
	delayNeg := -3

	cases := []struct {
		name string
		cfg  *TempMailConfig
		want int
	}{
		{name: "nil config uses default", cfg: nil, want: 15},
		{name: "missing value uses default", cfg: &TempMailConfig{}, want: 15},
		{name: "zero means no wait", cfg: &TempMailConfig{NextDelaySeconds: &delay0}, want: 0},
		{name: "keeps valid value", cfg: &TempMailConfig{NextDelaySeconds: &delay12}, want: 12},
		{name: "clamps high", cfg: &TempMailConfig{NextDelaySeconds: &delayHigh}, want: 300},
		{name: "negative falls back to default", cfg: &TempMailConfig{NextDelaySeconds: &delayNeg}, want: 15},
	}

	for _, tc := range cases {
		got := tc.cfg.PostSuccessDelaySeconds()
		if got != tc.want {
			t.Fatalf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

func TestTempMailMailboxCreateGap(t *testing.T) {
	delay0 := 0
	delay7 := 7

	cases := []struct {
		name string
		cfg  *TempMailConfig
		want time.Duration
	}{
		{name: "nil config uses default gap", cfg: nil, want: 15 * time.Second},
		{name: "missing value uses default gap", cfg: &TempMailConfig{}, want: 15 * time.Second},
		{name: "zero means immediate rotate", cfg: &TempMailConfig{NextDelaySeconds: &delay0}, want: 0},
		{name: "uses configured delay", cfg: &TempMailConfig{NextDelaySeconds: &delay7}, want: 7 * time.Second},
	}

	for _, tc := range cases {
		got := tc.cfg.MailboxCreateGap()
		if got != tc.want {
			t.Fatalf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
}

func TestIsPasswordlessUnavailable(t *testing.T) {
	body := `{"error":{"message":"Passwordless signup is unavailable. Please continue with a password."}}`
	if !isPasswordlessUnavailable(401, body) {
		t.Fatalf("expected passwordless unavailable to be detected")
	}
	if isPasswordlessUnavailable(400, body) {
		t.Fatalf("unexpected match for non-401 response")
	}
	if isPasswordlessUnavailable(401, `{"error":{"message":"other"}}`) {
		t.Fatalf("unexpected match for unrelated 401 response")
	}
}

func TestNormalizeRegisterPassword(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "blank uses default", input: "", want: defaultRegisterPassword},
		{name: "legacy default is upgraded", input: "Qwer1234!", want: defaultRegisterPassword},
		{name: "valid password is kept", input: "Abcd1234!XYZ", want: "Abcd1234!XYZ"},
		{name: "short custom password is rejected", input: "short123!", wantErr: true},
	}

	for _, tc := range cases {
		got, err := normalizeRegisterPassword(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: expected error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestAdjustedFlowDelayRange(t *testing.T) {
	minMs, maxMs := adjustedFlowDelayRange(false, 800, 2000)
	if minMs != 800 || maxMs != 2000 {
		t.Fatalf("non-temp mode should keep original delays, got %d-%d", minMs, maxMs)
	}

	minMs, maxMs = adjustedFlowDelayRange(true, 800, 2000)
	if minMs != 200 || maxMs != 500 {
		t.Fatalf("temp mode should shrink delays, got %d-%d", minMs, maxMs)
	}

	minMs, maxMs = adjustedFlowDelayRange(true, 100, 120)
	if minMs < 80 || maxMs < minMs {
		t.Fatalf("temp mode should keep a valid clamped range, got %d-%d", minMs, maxMs)
	}
}
