// Package id generates short human-typeable pool IDs and unique node IDs.
package id

import (
	"crypto/rand"
	"math/big"
)

// alphabet avoids visually ambiguous characters (0/O, 1/I).
const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// PoolID returns a 6-character pool identifier, e.g. "K4RT9X".
func PoolID() string {
	return random(6)
}

// NodeID returns a longer identifier unique enough to sort a stable device
// ring across the pool without coordination.
func NodeID() string {
	return random(12)
}

func random(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b[i] = alphabet[idx.Int64()]
	}
	return string(b)
}
