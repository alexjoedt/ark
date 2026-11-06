//go:build !linux && !darwin

package ark

// ark supports Linux and macOS only. This undefined name stops a build for
// any other platform with an error that names the reason.
var _ = arkSupportsOnlyLinuxAndMacOS
