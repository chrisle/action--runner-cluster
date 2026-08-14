// Package hostid derives a short, stable identifier for this machine.
//
// Several arc hosts can serve one GitHub account — that is how capacity adds
// up (two Macs at -max 4 give the macos labels a ceiling of 8). The hosts
// never talk to each other; they coordinate through GitHub, and this id is
// what keeps them apart there: runner names embed it so a host only reaps its
// own orphans, and webhook paths embed it so every host keeps its own hook
// instead of stealing the last one registered.
package hostid

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
)

// Length of the identifier in hex characters.
const Length = 6

// ID returns the identifier for this machine.
func ID() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "000000"
	}
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])[:Length]
}
