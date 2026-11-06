//go:build !darwin

package direct

import "syscall"

// defaultInterfaces leaves everything to the routes.
func defaultInterfaces() (v4, v6 int) { return 0, 0 }

func bindSocket(syscall.RawConn, string, int) error { return nil }
