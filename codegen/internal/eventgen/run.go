package eventgen

import (
	"io"
)

func Run(args []string, stdout io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	return run(args, stdout)
}
