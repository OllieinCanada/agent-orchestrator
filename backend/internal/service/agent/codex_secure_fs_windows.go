//go:build windows

package agent

import "os"

// Windows ACL ownership and link identity are validated by the no-reparse-point
// open path used by the account vault. FileInfo does not expose a portable UID.
func ownedByCurrentUser(os.FileInfo) bool { return true }
func hasSingleHardLink(os.FileInfo) bool  { return true }
