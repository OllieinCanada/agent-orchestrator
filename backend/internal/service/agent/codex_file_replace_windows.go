//go:build windows

package agent

import (
	"syscall"
	"unsafe"
)

const codexMoveFileReplaceExisting = 0x1

var codexMoveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func replaceCodexFile(source, target string) error {
	sourcePtr, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetPtr, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	ok, _, callErr := codexMoveFileExW.Call(
		uintptr(unsafe.Pointer(sourcePtr)),
		uintptr(unsafe.Pointer(targetPtr)),
		uintptr(codexMoveFileReplaceExisting),
	)
	if ok == 0 {
		return callErr
	}
	return nil
}
