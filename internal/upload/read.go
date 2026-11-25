// Package upload owns bounded consumption and closure of uploaded resources.
package upload

import (
	"errors"
	"io"
)

// ReadAndClose takes ownership of a nonnil file and reads at most limit bytes.
// A nonpositive limit reads no bytes and still closes the file. Partial bytes
// and both read/close errors are returned; callers must reject errored input.
// This helper does not classify oversize input: callers choose their limit.
func ReadAndClose(file io.ReadCloser, limit int64) ([]byte, error) {
	raw, readError := io.ReadAll(io.LimitReader(file, limit))
	closeError := file.Close()
	return raw, errors.Join(readError, closeError)
}
