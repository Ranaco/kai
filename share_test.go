package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe: %v", err)
	}
	os.Stderr = w
	defer func() {
		os.Stderr = orig
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close write end: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close read end: %v", err)
	}

	return string(data)
}

func TestRunShareNoArgsShowsUsage(t *testing.T) {
	output := captureStderr(t, func() {
		exitCode := runShare(nil)
		if exitCode != exitCodeSuccess {
			t.Fatalf("expected exit code %d, got %d", exitCodeSuccess, exitCode)
		}
	})

	if !strings.Contains(output, "Usage:") {
		t.Fatalf("expected usage banner in output, got %q", output)
	}
	if !strings.Contains(output, "kai share <source> <provider> [flags]") {
		t.Fatalf("expected positional usage in output, got %q", output)
	}
}

func TestRunShareMissingProviderReturnsUsageError(t *testing.T) {
	output := captureStderr(t, func() {
		exitCode := runShare([]string{"--from", "https://example.com/a.zip"})
		if exitCode != exitCodeUsage {
			t.Fatalf("expected exit code %d, got %d", exitCodeUsage, exitCode)
		}
	})

	if !strings.Contains(output, "--provider is required") {
		t.Fatalf("expected provider error in output, got %q", output)
	}
}
