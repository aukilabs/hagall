//go:build ignore

// Apply the reviewed disconnect-race backport to generated vendor output.
// Run from the repository root after go mod vendor; it is safe to repeat.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"strings"
)

const source = "vendor/github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay/relay.go"
const beforeSHA = "8078a452fd8d7a7aa4b62a1fb3b6046ca4511d5e217eea9066e3479d58cbcfab"
const afterSHA = "a9adaa5a0412784c5c846d65271223e1e3e89a89a7c9d867ad9fdc3d67e799a6"

const before = `	r.mx.Lock()
	_, ok := r.rsvp[p]
`

const after = `	r.mx.Lock()
	// A new connection can reserve after the first connectedness check. Recheck
	// while holding the same lock as handleReserve so stale disconnect cleanup
	// cannot remove an accepted replacement reservation.
	if n.Connectedness(p) == network.Connected {
		r.mx.Unlock()
		return
	}
	_, ok := r.rsvp[p]
`

func main() {
	check := flag.Bool("check", false, "verify the patch without changing files")
	flag.Parse()
	if err := patch(*check); err != nil {
		fmt.Fprintln(os.Stderr, "relay dependency patch:", err)
		os.Exit(1)
	}
}

func patch(check bool) error {
	modules, err := os.ReadFile("vendor/modules.txt")
	if err != nil {
		return err
	}
	if !strings.Contains(string(modules), "# github.com/libp2p/go-libp2p v0.41.1\n") {
		return fmt.Errorf("requires go-libp2p v0.41.1; review or retire this backport when upgrading")
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	switch fmt.Sprintf("%x", sha256.Sum256(data)) {
	case afterSHA:
		return nil
	case beforeSHA:
		if check {
			return fmt.Errorf("missing disconnect-race backport; run make go-vendor")
		}
	default:
		return fmt.Errorf("unexpected relay source; refusing an unreviewed patch")
	}
	if strings.Count(string(data), before) != 1 {
		return fmt.Errorf("disconnect patch must match exactly once")
	}
	patched := []byte(strings.Replace(string(data), before, after, 1))
	if fmt.Sprintf("%x", sha256.Sum256(patched)) != afterSHA {
		return fmt.Errorf("unexpected patched source checksum")
	}
	return os.WriteFile(source, patched, 0644)
}
