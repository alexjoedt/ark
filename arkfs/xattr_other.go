//go:build !linux && !darwin

package arkfs

func readXattrs(string) (map[string][]byte, error) { return nil, nil }

func writeXattrs(string, map[string][]byte) {} // not supported
