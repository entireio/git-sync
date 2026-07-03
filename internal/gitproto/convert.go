package gitproto

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// CloseOnce wraps a ReadCloser so repeated Close calls only close the
// underlying reader once. Strategies use it for pack readers that are closed
// both by PushPack and by the caller's retry/error cleanup, so double closes
// do not surface spurious failures. Passing an already-wrapped or nil reader
// returns it unchanged.
func CloseOnce(rc io.ReadCloser) io.ReadCloser {
	if rc == nil {
		return nil
	}
	if _, ok := rc.(*closeOnceReadCloser); ok {
		return rc
	}
	return &closeOnceReadCloser{ReadCloser: rc}
}

type closeOnceReadCloser struct {
	io.ReadCloser

	once sync.Once
}

func (c *closeOnceReadCloser) Close() error {
	var err error
	c.once.Do(func() {
		err = c.ReadCloser.Close()
	})
	if err != nil {
		return fmt.Errorf("close pack reader: %w", err)
	}
	return nil
}

// LimitPackReader wraps a ReadCloser with a byte limit. Shared across strategies.
func LimitPackReader(r io.ReadCloser, maxBytes int64) io.ReadCloser {
	if maxBytes <= 0 {
		return r
	}
	return &packLimitRC{ReadCloser: r, max: maxBytes}
}

type packLimitRC struct {
	io.ReadCloser

	max  int64
	read int64
}

func (r *packLimitRC) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.read += int64(n)
	if r.read > r.max {
		return n, fmt.Errorf("source pack exceeded max-pack-bytes limit (%d)", r.max)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("read: %w", err)
	}
	return n, err //nolint:wrapcheck // io.EOF must pass through for io.Reader contract
}
