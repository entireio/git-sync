package gitproto

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// This file holds shared io.ReadCloser wrappers for pack streams produced
// and consumed by this package.

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

// MaxAdvertisementBytes bounds a ref/capability advertisement read from a
// remote. The advertisement is accumulated in memory before it can be parsed,
// and its length is entirely up to the far end, so every transport needs the
// same ceiling: an endless stream would otherwise grow the buffer until the
// process died. 64 MiB is far above any real advertisement — a repository with
// a million refs advertises on the order of a few tens of MiB.
const MaxAdvertisementBytes = 64 << 20

// readCappedAdvertisement reads r to EOF, failing rather than allocating
// without bound if it yields more than MaxAdvertisementBytes. what names the
// thing being read, for the error message.
func readCappedAdvertisement(r io.Reader, what string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxAdvertisementBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	if int64(len(data)) > MaxAdvertisementBytes {
		return nil, fmt.Errorf("%s exceeds %d byte limit", what, MaxAdvertisementBytes)
	}
	return data, nil
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
