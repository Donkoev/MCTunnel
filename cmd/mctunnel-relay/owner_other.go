//go:build !unix

package main

import "os"

// copyOwner does nothing where files have no Unix owner (a relay.json edited on Windows).
func copyOwner(*os.File, os.FileInfo) error {
	return nil
}
