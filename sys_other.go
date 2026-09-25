//go:build !linux

package localhost

import (
	"errors"
	"runtime"
)

const supported = false

var errUnsupported = errors.New("the localhost module reads /proc and /sys, which " + runtime.GOOS + " does not have; set root to a copied Linux tree to test it")

func statfs(string) (fsUsage, error) { return fsUsage{}, errUnsupported }
