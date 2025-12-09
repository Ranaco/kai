//go:build darwin

package main

import _ "embed"

//go:embed frpc_darwin_amd64
var frpcBinary []byte
