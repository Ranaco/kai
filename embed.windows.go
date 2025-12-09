//go:build windows

package main

import _ "embed"

//go:embed frpc_windows_amd64.exe
var frpcBinary []byte
