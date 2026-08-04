// Physical file identity extraction for Unix task-log validation.
//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package task

import (
	"os"
	"syscall"
)

func physicalFileIdentityFromFile(_ *os.File, info os.FileInfo) physicalFileIdentity {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return physicalFileIdentity{}
	}
	return physicalFileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Valid: true} //nolint:unconvert // syscall.Stat_t field types vary by platform.
}
