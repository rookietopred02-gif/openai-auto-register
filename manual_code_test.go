package main

import (
	"path/filepath"
	"testing"
)

func TestParseManualCodeInputSinglePending(t *testing.T) {
	email, code, err := parseManualCodeInput("123456", []string{"user@example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if email != "user@example.com" || code != "123456" {
		t.Fatalf("unexpected result: %s %s", email, code)
	}
}

func TestParseManualCodeInputRequiresEmailForMultiplePending(t *testing.T) {
	_, _, err := parseManualCodeInput("123456", []string{"a@example.com", "b@example.com"})
	if err == nil {
		t.Fatal("expected error for ambiguous manual code input")
	}
}

func TestParseManualCodeInputExplicitEmail(t *testing.T) {
	email, code, err := parseManualCodeInput("user@example.com----654321", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if email != "user@example.com" || code != "654321" {
		t.Fatalf("unexpected result: %s %s", email, code)
	}
}

func TestInjectManualCodeStoresEntry(t *testing.T) {
	service := NewIntegratedIMAPService(filepath.Join(t.TempDir(), "codes.json"))
	entry, err := service.InjectManualCode("user@example.com", "123456", "manual")
	if err != nil {
		t.Fatalf("inject manual code: %v", err)
	}
	if entry.Email != "user@example.com" || entry.Code != "123456" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	stored, ok := service.PeekCode("user@example.com")
	if !ok || stored.Code != "123456" {
		t.Fatalf("expected injected code to be stored, got %+v", stored)
	}
}
