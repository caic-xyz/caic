// Physical file identity extraction for Windows task-log validation.
//go:build windows

package task

import (
	"os"

	"golang.org/x/sys/windows"
)

func physicalFileIdentityFromFile(file *os.File, _ os.FileInfo) physicalFileIdentity {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return physicalFileIdentity{}
	}
	return physicalFileIdentity{
		Device: uint64(info.VolumeSerialNumber),
		Inode:  uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
		Valid:  true,
	}
}
