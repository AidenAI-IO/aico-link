//go:build !linux && !darwin

package mcu

import "os"

func ownedByRoot(os.FileInfo) bool { return false }
