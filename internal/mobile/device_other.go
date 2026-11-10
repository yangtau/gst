//go:build !darwin

package mobile

import "errors"

// systemDevice has nothing to return: only Apple's systems make an
// interface for a process.
func systemDevice() (Device, error) {
	return nil, errors.New("this system gives a process no interface")
}
