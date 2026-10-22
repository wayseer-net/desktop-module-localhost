//go:build !linux

package localhost

// socketSource is nil: only Linux's sock_diag is read so far.
var socketSource func() ([]socket, string, error)
