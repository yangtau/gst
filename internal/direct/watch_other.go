//go:build !darwin && !(linux && !android)

package direct

// watchSystem does nothing here: the transport looks at the network every
// watchEvery, and an app may say when it changed (Transport.LookNow), as
// Android's does.
func watchSystem(func(), <-chan struct{}) {}
