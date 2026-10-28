// Opens delete-on-close projection spools so process exit cannot orphan their contents.

package history

import (
	"crypto/rand"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func openProjectionSpool(dir string) (*os.File, error) {
	name := filepath.Join(dir, ".caic-history-projection-"+rand.Text())
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(path,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_TEMPORARY|windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), name), nil
}
