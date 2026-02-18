package main

import (
	"bytes"
	_ "embed"
	"bufio"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const DefaultToken = "@st@r@nje"
const frpcConfigTemplate = `
serverAddr = "{{ .ServerAddr }}"
serverPort = {{ .ServerPort }}

[auth]
method = "token"
token  = "{{ .Token }}"

[[proxies]]
name      = "{{ .ProxyName }}"
type      = "{{ .Type }}"
localIP   = "{{ .LocalIP }}"
localPort = {{ .LocalPort }}
{{- if eq .Type "http" }}
subdomain = "{{ .Subdomain }}"
{{- end }}
{{- if eq .Type "tcp" }}
remotePort = {{ .RemotePort }}
{{- end }}
`

type TunnelConfig struct {
	ServerAddr string
	ServerPort int
	Token      string

	ProxyName  string
	Type       string
	LocalIP    string
	LocalPort  int
	Subdomain  string
	RemotePort int
}

type tunnelDefaults struct {
	Server     string
	ServerPort int
	Token      string
	LocalHost  string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "share" {
		os.Exit(runShare(os.Args[2:]))
	}

	if err := runTunnel(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatal(err)
	}
}

func runTunnel(args []string) error {
	defaults, err := loadTunnelDefaults()
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("kai", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		printMainUsage(fs)
	}

	sub := fs.String("subdomain", "", "Subdomain (required for http tunnel)")
	port := fs.Int("p", 0, "Local port")
	ttype := fs.String("type", "http", "Tunnel type: http or tcp")
	server := fs.String("server", defaults.Server, "FRPS server")
	serverPort := fs.Int("server-port", defaults.ServerPort, "FRPS port")
	token := fs.String("token", defaults.Token, "Auth token")
	localHost := fs.String("local-host", defaults.LocalHost, "Local host")
	remotePort := fs.Int("remote-port", 0, "Remote port (TCP only)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *token == "" {
		*token = DefaultToken
	}

	if *port == 0 {
		return fmt.Errorf("error: -p is required")
	}
	if *ttype == "http" && *sub == "" {
		return fmt.Errorf("error: --subdomain is required for HTTP tunnels")
	}
	if *ttype == "tcp" && *remotePort == 0 {
		return fmt.Errorf("error: --remote-port is required for TCP tunnels")
	}

	tmp, err := os.MkdirTemp("", "pclient-")
	if err != nil {
		return fmt.Errorf("temp dir error: %w", err)
	}
	defer os.RemoveAll(tmp)

	frpcName := "frpc"
	if runtime.GOOS == "windows" {
		frpcName = "frpc.exe"
	}

	frpcPath := filepath.Join(tmp, frpcName)
	if err := os.WriteFile(frpcPath, frpcBinary, 0755); err != nil {
		return fmt.Errorf("write frpc error: %w", err)
	}

	cfg := TunnelConfig{
		ServerAddr: *server,
		ServerPort: *serverPort,
		Token:      *token,
		ProxyName:  fmt.Sprintf("%s-%d-%d", *ttype, *port, time.Now().Unix()),
		Type:       *ttype,
		LocalIP:    *localHost,
		LocalPort:  *port,
		Subdomain:  *sub,
		RemotePort: *remotePort,
	}

	var buf bytes.Buffer
	tmpl := template.Must(template.New("cfg").Parse(frpcConfigTemplate))
	if err := tmpl.Execute(&buf, cfg); err != nil {
		return fmt.Errorf("render config error: %w", err)
	}

	configPath := filepath.Join(tmp, "frpc.toml")
	if err := os.WriteFile(configPath, buf.Bytes(), 0600); err != nil {
		return fmt.Errorf("write config error: %w", err)
	}

	cmd := exec.Command(frpcPath, "-c", configPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		<-sig
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	log.Println("Starting tunnel...")
	if *ttype == "http" {
		log.Printf("Tunnel is running! Access it at %s.p.ranax.co\nPress Cmd+C to stop client.", *sub)
	} else {
		log.Printf("Tunnel is running! Access it at p.ranax.co:%d\nPress Cmd+C to stop client.", *remotePort)
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("frpc exited: %w", err)
	}
	return nil
}

func printMainUsage(fs *flag.FlagSet) {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  kai [flags]")
	fmt.Fprintln(os.Stderr, "  kai share <source> <provider> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  share    Upload a URL or local file to a provider and print a share URL")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Tunnel Flags:")
	fs.PrintDefaults()
}

func loadTunnelDefaults() (tunnelDefaults, error) {
	defaults := tunnelDefaults{
		Server:     "p.ranax.co",
		ServerPort: 7000,
		Token:      "",
		LocalHost:  "127.0.0.1",
	}

	configPath, err := resolveConfigPath()
	if err != nil {
		return defaults, err
	}
	if configPath == "" {
		return defaults, nil
	}

	loaded, err := parseTunnelDefaultsFromConfig(configPath)
	if err != nil {
		return defaults, fmt.Errorf("failed to parse config %q: %w", configPath, err)
	}
	if loaded.Server != "" {
		defaults.Server = loaded.Server
	}
	if loaded.ServerPort > 0 {
		defaults.ServerPort = loaded.ServerPort
	}
	if loaded.Token != "" {
		defaults.Token = loaded.Token
	}
	if loaded.LocalHost != "" {
		defaults.LocalHost = loaded.LocalHost
	}
	return defaults, nil
}

func resolveConfigPath() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("KAI_CONFIG")); configured != "" {
		if _, err := os.Stat(configured); err == nil {
			return configured, nil
		} else if errors.Is(err, os.ErrNotExist) {
			return "", nil
		} else {
			return "", err
		}
	}

	if _, err := os.Stat("config.toml"); err == nil {
		return "config.toml", nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil
	}
	homeConfigPath := filepath.Join(home, ".kai", "config.toml")
	if _, err := os.Stat(homeConfigPath); err == nil {
		return homeConfigPath, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	return "", nil
}

func parseTunnelDefaultsFromConfig(configPath string) (tunnelDefaults, error) {
	file, err := os.Open(configPath)
	if err != nil {
		return tunnelDefaults{}, err
	}
	defer file.Close()

	var out tunnelDefaults
	section := ""
	scanner := bufio.NewScanner(file)
	lineNo := 0

	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}

		key, rawValue, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = normalizeTomlKey(key)
		value := strings.TrimSpace(rawValue)

		switch section {
		case "", "forwarding":
			switch key {
			case "server":
				str, err := parseTomlString(value)
				if err != nil {
					return tunnelDefaults{}, fmt.Errorf("line %d: %w", lineNo, err)
				}
				out.Server = str
			case "server_port":
				num, err := parseTomlInt(value)
				if err != nil {
					return tunnelDefaults{}, fmt.Errorf("line %d: %w", lineNo, err)
				}
				out.ServerPort = num
			case "local_host":
				str, err := parseTomlString(value)
				if err != nil {
					return tunnelDefaults{}, fmt.Errorf("line %d: %w", lineNo, err)
				}
				out.LocalHost = str
			}
		case "auth":
			if key == "token" {
				str, err := parseTomlString(value)
				if err != nil {
					return tunnelDefaults{}, fmt.Errorf("line %d: %w", lineNo, err)
				}
				out.Token = str
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return tunnelDefaults{}, err
	}
	return out, nil
}

func normalizeTomlKey(key string) string {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	return normalized
}

func parseTomlString(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty value")
	}
	if strings.HasPrefix(raw, "\"") && strings.HasSuffix(raw, "\"") && len(raw) >= 2 {
		return strings.Trim(raw, "\""), nil
	}
	if strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'") && len(raw) >= 2 {
		return strings.Trim(raw, "'"), nil
	}
	// bare values are accepted for simple compatibility.
	return strings.TrimSpace(raw), nil
}

func parseTomlInt(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	return value, nil
}
