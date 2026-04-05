// Package autoupdate provides binary version detection and nightly auto-update.
package autoupdate

import (
	"runtime/debug"
	"strings"
)

// Version is the running binary's version from Go's embedded build info.
// Tagged builds return e.g. "1.2.3". Dev builds return "devel-abc1234".
// Appends "-dirty" when built from a modified working tree.
var Version = initVersion()

func initVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var revision string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return formatVersion(bi.Main.Version, revision, dirty)
}

func formatVersion(moduleVersion, revision string, dirty bool) string {
	suffix := ""
	if dirty {
		suffix = "+dirty"
	}
	if moduleVersion == "" || moduleVersion == "(devel)" {
		if revision == "" {
			return ""
		}
		short := revision
		if len(short) > 8 {
			short = short[:8]
		}
		return "devel-" + short + suffix
	}
	v := strings.TrimPrefix(moduleVersion, "v")
	if strings.HasSuffix(v, "+dirty") {
		return v
	}
	return v + suffix
}
