//go:build !linux && !darwin

package localhost

import (
	"errors"
	"runtime"
	"wayseer/pkg/sdk"
)

const supported = false

var errUnsupported = errors.New("the localhost module reads /proc and /sys, which " + runtime.GOOS + " does not have; set root to a copied Linux tree to test it")

func statfs(string) (fsUsage, error) { return fsUsage{}, errUnsupported }

// nativeSource is nil: there is no reader for this OS's own interfaces yet.
func nativeSource(*reader) (source, []sdk.Metric) { return nil, nil }
