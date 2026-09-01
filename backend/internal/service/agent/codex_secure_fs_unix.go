//go:build !windows

package agent

import (
	"os"
	"syscall"
)

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	euid := os.Geteuid()
	return ok && euid >= 0 && uint64(stat.Uid) == uint64(euid)
}

func hasSingleHardLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
