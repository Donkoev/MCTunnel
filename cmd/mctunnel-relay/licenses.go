package main

import (
	"fmt"
	"io"
	"strings"

	"mctunnel/server/internal/kcp"
)

// relayLicense is the relay's own licence (LICENSE in the repository).
const relayLicense = `MIT License

Copyright (c) 2026 Ramazan Donkoev

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`

// printLicenses writes the relay's licence and the notice of KCP, which the relay contains a
// port of: `mctunnel-relay licenses`.
func printLicenses(w io.Writer) {
	rule := strings.Repeat("-", 72)
	fmt.Fprintf(w, "MCTunnel relay %s\nhttps://github.com/Donkoev/MCTunnel\n\n%s\n%s\n\n", version, relayLicense, rule)
	fmt.Fprintf(w, "The relay contains a port of KCP (internal/kcp):\n\n%s", kcp.License)
}
