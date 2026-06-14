// The harness binary. Everything lives in harness/ (the product) on
// top of agent (the core) and provider (the wire seam); this file only exists
// so `go build ./cmd/sesh` has a main.
package main

import "github.com/mike-diff/sesh/harness"

func main() { harness.Main() }
