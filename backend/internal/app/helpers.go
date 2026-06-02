// Small startup helper functions.

package app

import (
	"errors"
	"fmt"
	"strings"
)

func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, errors.New("odd length hex string")
	}
	b := make([]byte, len(s)/2)
	for i := range b {
		hi, lo := hexNibble(s[2*i]), hexNibble(s[2*i+1])
		if hi < 0 || lo < 0 {
			return nil, fmt.Errorf("invalid hex character at position %d", 2*i)
		}
		b[i] = byte(hi<<4 | lo) //nolint:gosec // G115: hi and lo are 0-15 from hexNibble, no overflow
	}
	return b, nil
}

func hexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

func parseAllowedUsers(csv string) map[string]struct{} {
	if csv == "" {
		return nil
	}
	m := make(map[string]struct{})
	for u := range strings.SplitSeq(csv, ",") {
		if u = strings.TrimSpace(u); u != "" {
			m[strings.ToLower(u)] = struct{}{}
		}
	}
	return m
}
