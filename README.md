# Kai – Cross-Platform FRPC Wrapper

Kai is a cross-platform tunneling client written in Go.
It wraps the `frpc` executable and embeds the appropriate binary for each platform using Go build tags.
Kai provides a simplified CLI for creating HTTP subdomain tunnels and TCP tunnels to an FRP server.

The goal is to offer a minimal, self-contained client similar to ngrok, but powered by a user-hosted FRP server.

---

## Overview

Kai embeds platform-specific `frpc` binaries directly into the compiled executable.
At runtime, Kai:

1. Creates a temporary working directory.
2. Extracts the embedded `frpc` binary to the filesystem.
3. Generates a temporary `frpc.toml` configuration.
4. Executes the embedded `frpc` with the generated configuration.
5. Cleans up the temporary files on exit.

This allows users to run Kai without installing `frpc` manually.

## [FRP](https://github.com/fatedier/frp)

FRP is being used to connect the client to the server and handle proxy connections for the
given domain.

One can reference the frpc.toml and frps.toml to see how the client and server are configured.

---

## Project Structure

```
.
├── embed.darwin.go          # macOS frpc embed (build-tagged)
├── embed.linux.go           # Linux frpc embed (build-tagged)
├── embed.windows.go         # Windows frpc embed (build-tagged)
│   // Binaries can be download from fatedier/frp
├── frpc_darwin_amd64        # local binary used for embedding (not committed)
├── frpc_linux_amd64         # local binary used for embedding (not committed)
├── frpc_windows_amd64.exe   # local binary used for embedding (not committed)
│
├── index.go                 # main application source
├── go.mod
└── kai                      # compiled binary (not committed)
```

The FRPC binaries are intentionally excluded from Git.
During a build, they must exist in the project directory so that `go:embed` can include them.

---

## Embedding Mechanism

Go build tags ensure that only the correct FRPC binary is embedded for the target platform.

Example (Linux):

```go
//go:build linux

package main

import _ "embed"

//go:embed frpc_linux_amd64
var frpcBinary []byte
```

The Windows and macOS versions follow the same pattern with different build tags and filenames.

---

## Building Kai

### Linux Build

```
GOOS=linux GOARCH=amd64 go build -o kai .
```

Requires `frpc_linux_amd64` to be present in the project directory.

---

### macOS Build

```
GOOS=darwin GOARCH=amd64 go build -o kai .
```

Requires `frpc_darwin_amd64` to be present.

---

### Windows Build

```
GOOS=windows GOARCH=amd64 go build -o kai.exe .
```

Requires `frpc_windows_amd64.exe` to be present.

---

## Command Usage

### HTTP Tunnel

Expose a local HTTP service using a subdomain.

```
kai --subdomain demo -p 3000
```

This maps:

```
localhost:3000  -->  demo.p.ranax.co
```

Requires your FRP server to be configured with:

```
subDomainHost = "p.ranax.co"
```

---

### TCP Tunnel

Expose a local TCP port (for example SSH).

```
kai --type tcp -p 22 --remote-port 22022
```

Remote access would be:

```
ssh user@p.ranax.co -p 22022
```

---

## Available Flags

| Flag            | Description                                           |
| --------------- | ----------------------------------------------------- |
| `--subdomain`   | Subdomain for HTTP tunnels (required for HTTP mode)   |
| `--type`        | Tunnel type. Options: `http` (default), `tcp`         |
| `--port`, `-p`  | Local port to forward                                 |
| `--remote-port` | Remote FRP port (required for TCP mode)               |
| `--server`      | FRPS server address (default: `p.ranax.co`)           |
| `--server-port` | FRPS bind port (default: `7000`)                      |
| `--local-host`  | Local host IP (default: `127.0.0.1`)                  |
| `--token`       | FRP authentication token (defaults to built-in token) |

---

## Execution Flow

1. User runs a Kai command.
2. Kai parses CLI flags and validates input.
3. A temporary directory is created under the OS temp path.
4. The correct embedded `frpc` binary is written to this directory.
5. A temporary `frpc.toml` file is generated based on the CLI parameters.
6. Kai executes the embedded `frpc` with `exec.Command`.
7. On SIGINT (Ctrl+C), Kai terminates the `frpc` child process.
8. Temporary files are removed on exit.

---

## System Requirements

* Go 1.21+
* FRP server (frps) configured with:

  * token-based authentication
  * `subDomainHost` enabled for HTTP tunnels
  * vhost HTTP/HTTPS ports open and listening

---

## Intended Usage

Kai is designed for:

* developers needing quick tunnels
* internal tools
* private infrastructure
* self-hosted alternatives to ngrok or Cloudflare Tunnel

Kai is not intended to replace the full FRP client but to provide a simplified wrapper for specific common workflows.

---

## License

Specify a license if applicable (MIT, Apache, etc.).
