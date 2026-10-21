//go:build !windows

package audio

func openCapture(device string, loopback bool) (capturer, error) { return nil, ErrNoBackend }

func listDevices(loopback bool) ([]string, error) { return nil, ErrNoBackend }
