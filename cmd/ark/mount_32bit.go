//go:build linux && (386 || arm || mips || mipsle)

package main

import (
	"context"
	"errors"

	"github.com/urfave/cli/v3"
)

// mountCommand is a stub on 32-bit Linux, where go-fuse v2.12.1 does not
// build (its splice package assumes a 64-bit int). Drop it once a go-fuse
// release builds there.
func mountCommand() *cli.Command {
	return &cli.Command{
		Name:      "mount",
		Usage:     "serve every snapshot read-only through FUSE",
		ArgsUsage: "<dir>",
		Action: func(context.Context, *cli.Command) error {
			return errors.New("mount is not available on 32-bit Linux")
		},
	}
}
