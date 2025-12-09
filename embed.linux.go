//go:build linux

package main

import _ "embed"

//go:embed frpc_linux_amd64
var frpcBinary []byte
