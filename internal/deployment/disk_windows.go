//go:build windows

package deployment

import "golang.org/x/sys/windows"

func diskFreeBytes(path string) (uint64, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free uint64
	err = windows.GetDiskFreeSpaceEx(pointer, &free, nil, nil)
	return free, err
}
