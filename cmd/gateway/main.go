// Command gateway is Spillway's data plane. Until M1 it only prints its
// version, so the image build and its non-root, read-only checks (SEC-10) have
// a binary to run.
package main

import "fmt"

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	fmt.Println("spillway-gateway", version)
}
