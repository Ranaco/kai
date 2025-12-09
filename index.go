package main

import (
	"bytes"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
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

func main() {
	sub := flag.String("subdomain", "", "Subdomain (required for http tunnel)")
	port := flag.Int("p", 0, "Local port")
	ttype := flag.String("type", "http", "Tunnel type: http or tcp")

	server := flag.String("server", "p.ranax.co", "FRPS server")
	serverPort := flag.Int("server-port", 7000, "FRPS port")
	token := flag.String("token", "", "Auth token")

	localHost := flag.String("local-host", "127.0.0.1", "Local host")
	remotePort := flag.Int("remote-port", 0, "Remote port (TCP only)")

	flag.Parse()

	if *token == "" {
    	*token = DefaultToken
	}

	if *port == 0 {
		log.Fatal("Error: --port is required")
	}
	if *ttype == "http" && *sub == "" {
		log.Fatal("Error: --subdomain is required for HTTP tunnels")
	}
	if *ttype == "tcp" && *remotePort == 0 {
		log.Fatal("Error: --remote-port is required for TCP tunnels")
	}

	tmp, err := os.MkdirTemp("", "pclient-")
	if err != nil {
		log.Fatalf("temp dir error: %v", err)
	}
	defer os.RemoveAll(tmp)

	frpcName := "frpc"
	if runtime.GOOS == "windows" {
		frpcName = "frpc.exe"
	}

	frpcPath := filepath.Join(tmp, frpcName)
	if err := os.WriteFile(frpcPath, frpcBinary, 0755); err != nil {
		log.Fatalf("write frpc error: %v", err)
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
	tmpl.Execute(&buf, cfg)

	configPath := filepath.Join(tmp, "frpc.toml")
	if err := os.WriteFile(configPath, buf.Bytes(), 0600); err != nil {
		log.Fatalf("write config error: %v", err)
	}

	cmd := exec.Command(frpcPath, "-c", configPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	log.Println("Starting tunnel...")
	log.Println(fmt.Sprintf("Tunnel is running! Access it at %s.p.ranax.co | \nPress Cmd+C to stop client.", *sub))
	if err := cmd.Run(); err != nil {
		log.Fatalf("frpc exited: %v", err)
	}
}
