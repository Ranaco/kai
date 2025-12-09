# Kai – FRP-Based Cross-Platform Tunneling Client

Kai is a cross-platform tunneling client written in Go.  
It embeds the [FRP](https://github.com/fatedier/frp) binary inside the executable and provides a simplified CLI for creating HTTP and TCP tunnels to a self-hosted FRPS server.

Kai is intended as a lightweight, self-contained alternative to tools like ngrok, while giving users complete control of the infrastructure.

This document explains:

1. How the server (FRPS) is configured  
2. Required DNS setup  
3. How the Kai client embeds and launches FRPC  
4. How HTTP and TCP tunnels work end-to-end  
5. How to build and use the system  

---

# 1. System Overview

Kai consists of two components:

1. **FRPS (server)** running on a publicly accessible machine  
2. **Kai (client)** which embeds FRPC and establishes tunnels

Tunnel workflow:

- Kai generates an FRPC configuration at runtime  
- It extracts an embedded FRPC binary for the current OS  
- FRPC connects to FRPS using a persistent connection  
- FRPS exposes public endpoints such as:  
  - `https://demo.<YOUR DOMAIN>` for HTTP tunnels  
  - `<YOUR DOMAIN>:<REMOTE PORT>` for TCP tunnels  
- Traffic is routed back to the local machine running Kai  

This allows developers or internal users to expose services without configuring FRPC directly.

---

# 2. Server Requirements (FRPS)

You must host FRPS on a public server (VPS, cloud instance, or bare metal).

Typical configuration:

```
Hostname: <YOUR DOMAIN>
IP:       <YOUR SERVER IP>
FRP:      Same version for FRPS and FRPC
```

### Required open ports

| Port | Description |
|------|-------------|
| 7000 | FRPS bind port (client connections) |
| 80   | HTTP vHost (public HTTP tunnels) |
| 443  | HTTPS vHost (public HTTPS tunnels) |
| Optional: 8080 | Internal vHost if using a reverse proxy |

[!NOTE]
FRPS can be run behind a reverse proxy such as Nginx or Caddy to provide TLS termination and additional features.

---

## 2.1 Example `frps.toml`

```
bindPort = 7000

vhostHTTPPort  = 80
vhostHTTPSPort = 443

subDomainHost = "<YOUR DOMAIN>"

[auth]
method = "token"
token  = "<YOUR FRP TOKEN>"

[webServer]
addr = "0.0.0.0"
port = 7500
user = "admin"
password = "adminpass"
```

Key field:

```
subDomainHost = "<YOUR DOMAIN>"
```

This enables subdomain-based HTTP tunnels such as:

```
demo.<YOUR DOMAIN>
app1.<YOUR DOMAIN>
anything.<YOUR DOMAIN>
```

---

# 3. DNS Configuration

Your DNS provider should contain:

```
A   <YOUR DOMAIN>          <YOUR SERVER IP>
A   *.<YOUR DOMAIN>        <YOUR SERVER IP>
```

The wildcard record enables dynamic subdomains to be created automatically.

---

# 4. Kai Client Architecture

Kai embeds FRPC using Go's `//go:embed` feature with build tags.

### Build-time

- Linux build embeds `frpc_linux_amd64`
- macOS build embeds `frpc_darwin_amd64`
- Windows build embeds `frpc_windows_amd64.exe`

### Runtime

1. CLI arguments are parsed  
2. A temporary directory is created  
3. The embedded FRPC binary is written to the temporary directory  
4. Kai generates a temporary `frpc.toml` configuration  
5. Kai executes FRPC with this configuration  
6. SIGINT (Ctrl+C) is forwarded to FRPC  
7. Temporary files are removed after exit  

This provides a single portable executable per OS.

---

# 5. Tunnel Operation Flow

## 5.1 HTTP Subdomain Tunnel

Example command:

```
kai --subdomain demo -p 3000
```

Flow:

```
Browser → demo.<YOUR DOMAIN>:80
                ↓
             FRPS
                ↓
         FRPC (Kai)
                ↓
       Local service (localhost:3000)
```

Detailed:

1. User or browser accesses `demo.<YOUR DOMAIN>`.
2. DNS resolves to your FRPS server.
3. FRPS reads the HTTP Host header.
4. FRPS finds a matching FRPC session that registered `subdomain = "demo"`.
5. Traffic is forwarded through the FRPC tunnel to `localhost:3000`.

---

## 5.2 TCP Tunnel

Example:

```
kai --type tcp -p 22 --remote-port 22022
```

Flow:

```
Client → <YOUR DOMAIN>:22022
                 ↓
               FRPS
                 ↓
           FRPC (Kai)
                 ↓
   Local machine running service
```

Typical use cases include SSH, databases, or custom protocols.

---

# 6. Project Structure

```
embed.linux.go          # Linux frpc embed
embed.windows.go        # Windows frpc embed
embed.darwin.go         # macOS frpc embed
│
frpc_linux_amd64        # Local binary (not committed)
frpc_windows_amd64.exe  # Local binary (not committed)
frpc_darwin_amd64       # Local binary (not committed)
│
index.go                # Main Kai application
go.mod
kai (compiled binary)   # Not committed
```

The FRPC binaries must exist locally during build but are excluded from version control.

---

# 7. Building Kai

## Linux Build

```
GOOS=linux GOARCH=amd64 go build -o kai .
```

Requires `frpc_linux_amd64`.

## macOS Build

```
GOOS=darwin GOARCH=amd64 go build -o kai .
```

Requires `frpc_darwin_amd64`.

## Windows Build

```
GOOS=windows GOARCH=amd64 go build -o kai.exe .
```

Requires `frpc_windows_amd64.exe`.

---

# 8. Usage Examples

## HTTP Tunnel

```
kai --subdomain demo -p 3000
```

Exposes:

```
localhost:3000 → https://demo.<YOUR DOMAIN>
```

## TCP Tunnel

```
kai --type tcp -p 22 --remote-port 22022
```

Exposes:

```
localhost:22 → <YOUR DOMAIN>:22022
```

## Custom server address

```
kai --server <YOUR DOMAIN> --server-port 7000 --subdomain test -p 8080
```

---

# 9. Authentication

Kai uses a default token defined inside the source code unless overridden using:

```
--token <TOKEN>
```

This must match the value in `frps.toml`:

```
[auth]
method = "token"
token  = "<YOUR FRP TOKEN>"
```

---

# 10. System Summary

- FRPS acts as the central routing point for public traffic  
- Kai simplifies FRPC usage by embedding the binary and generating configs dynamically  
- DNS wildcard entries allow dynamic subdomain HTTP routing  
- The system provides a self-contained tunneling solution without external dependencies  
