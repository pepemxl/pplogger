//go:build !unix

package main

import "os"

// inodeOf returns 0 on platforms that don't expose unix-style inodes; the
// tail loop falls back to size-based rotation detection.
func inodeOf(fi os.FileInfo) uint64 { return 0 }
