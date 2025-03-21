//go:build !windows
// +build !windows

package tea

import (
	"io"

	"github.com/muesli/cancelreader"
)

func newInputReader(r io.Reader, _ bool) (cancelreader.CancelReader, error) {
	return cancelreader.NewReader(r)
}
