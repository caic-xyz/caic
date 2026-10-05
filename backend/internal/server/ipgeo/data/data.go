// Package data defines the on-disk named IP-origin cache.
package data

import "time"

// Entry contains the origin CIDRs and their refresh timestamp.
type Entry struct {
	Updated  time.Time `json:"updated"`
	Prefixes []string  `json:"prefixes"`
}
