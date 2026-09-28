//go:build unix

package main

import (
	"os"
	"syscall"
)

// copyOwner gives f the owner and group of the file described by fi (root:mctunnel for the
// installed configuration, which the relay reads as mctunnel).
func copyOwner(f *os.File, fi os.FileInfo) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return f.Chown(int(st.Uid), int(st.Gid))
	}
	return nil
}
