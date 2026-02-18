package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTunnelDefaultsFromConfig(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.toml")
	content := `
[forwarding]
server = "frp.example.com"
server_port = 7100
local_host = "127.0.0.2"

[auth]
token = "abc123"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	got, err := parseTunnelDefaultsFromConfig(cfgPath)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	if got.Server != "frp.example.com" {
		t.Fatalf("server mismatch: got %q", got.Server)
	}
	if got.ServerPort != 7100 {
		t.Fatalf("server_port mismatch: got %d", got.ServerPort)
	}
	if got.LocalHost != "127.0.0.2" {
		t.Fatalf("local_host mismatch: got %q", got.LocalHost)
	}
	if got.Token != "abc123" {
		t.Fatalf("token mismatch: got %q", got.Token)
	}
}

func TestResolveConfigPathPriority(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() {
		_ = os.Chdir(oldWD)
	}()

	oldHome := os.Getenv("HOME")
	oldKaiConfig := os.Getenv("KAI_CONFIG")
	defer func() {
		_ = os.Setenv("HOME", oldHome)
		if oldKaiConfig == "" {
			_ = os.Unsetenv("KAI_CONFIG")
		} else {
			_ = os.Setenv("KAI_CONFIG", oldKaiConfig)
		}
	}()

	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(filepath.Join(homeDir, ".kai"), 0o755); err != nil {
		t.Fatalf("mkdir home config dir: %v", err)
	}
	homeCfg := filepath.Join(homeDir, ".kai", "config.toml")
	if err := os.WriteFile(homeCfg, []byte(""), 0o600); err != nil {
		t.Fatalf("write home config: %v", err)
	}
	if err := os.Setenv("HOME", homeDir); err != nil {
		t.Fatalf("set HOME: %v", err)
	}

	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir project: %v", err)
	}

	got, err := resolveConfigPath()
	if err != nil {
		t.Fatalf("resolve config (home): %v", err)
	}
	if got != homeCfg {
		t.Fatalf("expected home config %q, got %q", homeCfg, got)
	}

	localCfg := filepath.Join(projectDir, "config.toml")
	if err := os.WriteFile(localCfg, []byte(""), 0o600); err != nil {
		t.Fatalf("write local config: %v", err)
	}

	got, err = resolveConfigPath()
	if err != nil {
		t.Fatalf("resolve config (local): %v", err)
	}
	if got != "config.toml" {
		t.Fatalf("expected local config path %q, got %q", "config.toml", got)
	}

	envCfg := filepath.Join(tmpDir, "env.toml")
	if err := os.WriteFile(envCfg, []byte(""), 0o600); err != nil {
		t.Fatalf("write env config: %v", err)
	}
	if err := os.Setenv("KAI_CONFIG", envCfg); err != nil {
		t.Fatalf("set KAI_CONFIG: %v", err)
	}

	got, err = resolveConfigPath()
	if err != nil {
		t.Fatalf("resolve config (env): %v", err)
	}
	if got != envCfg {
		t.Fatalf("expected env config %q, got %q", envCfg, got)
	}
}

func captureStderrForIndexTests(t *testing.T, fn func()) string {
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

func TestRunTunnelHelpIncludesShareCommand(t *testing.T) {
	output := captureStderrForIndexTests(t, func() {
		err := runTunnel([]string{"--help"})
		if !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("expected flag.ErrHelp, got %v", err)
		}
	})

	if !strings.Contains(output, "Commands:") {
		t.Fatalf("expected command section in help output, got %q", output)
	}
	if !strings.Contains(output, "share") {
		t.Fatalf("expected share command in help output, got %q", output)
	}
}
