package bootstrap

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/go-git/go-git/v6/plumbing/format/packfile"
)

// ErrPackUploadAborted is returned from packStreamObserver.Read when
// the configured aborter says the upload is projected to exceed the
// target body limit. Surfaces up through the HTTP transport as a
// generic body-read error; bootstrap's push-failed branch checks the
// observer's Aborted() flag to distinguish "we cut it" from a
// server-side 413 / 500 / network failure.
var ErrPackUploadAborted = errors.New("pack upload aborted early: projected to exceed target body limit")

// packStreamObserver wraps the pack stream handed to PushPack with two
// instruments:
//
//  1. a byte counter (replacing the simpler packReadCounter), so the
//     bootstrap loop still learns how much data went up before a 413
//     for post-rejection sizing;
//  2. a streaming pack-format observer that tees the bytes through
//     packfile.Scanner running on a goroutine, exposing how many of
//     the pack header's advertised objects have actually been sent
//     across the wire so far.
//
// The (objects-sent, bytes-sent, total-objects) triple lets later
// callers project the real size before the server cuts the upload —
// which is the prerequisite for a mid-stream early abort. Until that
// abort path is added, the observer is a strict superset of the
// previous packReadCounter and only reports observed counters.
//
// The Scanner runs on a goroutine reading from the tee'd pipe; if it
// falls behind the HTTP upload, the unbuffered pipe back-pressures the
// upload until Scanner catches up. zlib decoding inside Scanner is the
// dominant cost and runs at 200–500 MB/s per core on modern hardware,
// which leaves substantial headroom over typical wire upload speeds.
// If profiling later shows the observer bottlenecking real pushes, the
// fix is a custom format walker that skips Scanner's per-object SHA-1
// hashing — but the zlib walk itself is unavoidable because pack
// objects don't record their compressed size.
type packStreamObserver struct {
	src io.ReadCloser
	tee io.Reader
	pw  *io.PipeWriter

	bytes        atomic.Int64
	objectsSent  atomic.Int64
	totalObjects atomic.Int64

	headerOnce  sync.Once
	headerReady chan struct{}
	done        chan struct{}
	scannerErr  atomic.Pointer[error]

	aborter aborterFunc
	aborted atomic.Bool
}

// aborterFunc is consulted on every Read to decide whether the upload
// should be cancelled mid-stream. Receives the latest counters so it
// can project the final pack size from the bytes-per-object ratio so
// far. Return true to abort the upload; the observer surfaces
// ErrPackUploadAborted from its next Read and refuses to give up
// further bytes.
type aborterFunc func(bytesSent, objectsSent, totalObjects int64) bool

func newPackStreamObserver(src io.ReadCloser) *packStreamObserver {
	pr, pw := io.Pipe()
	obs := &packStreamObserver{
		src:         src,
		pw:          pw,
		headerReady: make(chan struct{}),
		done:        make(chan struct{}),
	}
	// Tee through a best-effort writer: once the consume goroutine stops and
	// closes pr (on a Scanner error or after a malformed pack), writes to pw
	// would fail with io.ErrClosedPipe and TeeReader would surface that from
	// Read, aborting the live upload. Observation is non-fatal, so absorb the
	// failure and keep the bytes flowing to the server.
	obs.tee = io.TeeReader(src, &bestEffortWriter{w: pw})
	go obs.consume(pr)
	return obs
}

// bestEffortWriter forwards bytes to the observer pipe but never propagates a
// write failure: after the first error it silently drops subsequent writes and
// always reports success, so the wrapping TeeReader cannot turn a stopped
// observer into a failed upload. Touched only by the upload's Read goroutine.
type bestEffortWriter struct {
	w      io.Writer
	broken bool
}

func (b *bestEffortWriter) Write(p []byte) (int, error) {
	if b.broken {
		return len(p), nil
	}
	if _, err := b.w.Write(p); err != nil {
		b.broken = true
	}
	return len(p), nil
}

func (o *packStreamObserver) Read(p []byte) (int, error) {
	if o.aborted.Load() {
		return 0, ErrPackUploadAborted
	}
	n, err := o.tee.Read(p)
	if n > 0 {
		o.bytes.Add(int64(n))
	}
	if err == nil && o.aborter != nil {
		if o.aborter(o.bytes.Load(), o.objectsSent.Load(), o.totalObjects.Load()) {
			o.aborted.Store(true)
			return n, ErrPackUploadAborted
		}
	}
	return n, err //nolint:wrapcheck // Read must preserve io.EOF for io.Reader contract
}

// SetAborter registers a function that decides, on each Read, whether
// the upload should be cancelled mid-stream. Must be called before the
// observer starts receiving Reads (i.e. before being handed to
// PushPack); not safe to swap concurrently with active Reads.
func (o *packStreamObserver) SetAborter(f aborterFunc) {
	o.aborter = f
}

// Aborted reports whether the aborter triggered during this upload.
// Stays true after the first abort even though subsequent Reads keep
// returning the sentinel error; callers should check this flag to
// distinguish a self-imposed early stop from a server-side 4xx/5xx.
func (o *packStreamObserver) Aborted() bool {
	return o.aborted.Load()
}

// Close releases the observer's pipe so the Scanner goroutine drains
// cleanly, then closes the underlying source. Idempotent on the source
// side (the wrapped ReadCloser may itself be a closeOnce wrapper).
func (o *packStreamObserver) Close() error {
	_ = o.pw.Close()
	<-o.done
	if err := o.src.Close(); err != nil {
		return fmt.Errorf("close pack source: %w", err)
	}
	return nil
}

// HeaderReady returns a channel that closes once the pack header has
// been parsed and TotalObjects is populated. Callers that need the
// total before bytes start flowing can wait on this; callers that just
// poll TotalObjects opportunistically can ignore it.
func (o *packStreamObserver) HeaderReady() <-chan struct{} {
	return o.headerReady
}

// Bytes returns the cumulative bytes pulled from the source so far.
func (o *packStreamObserver) Bytes() int64 {
	return o.bytes.Load()
}

// ObjectsSent returns the number of objects fully observed by the
// Scanner. Scanner emits one ObjectSection per object after fully
// walking that object's zlib stream, so this is "objects whose bytes
// have all been read by us" — not "objects whose bytes are confirmed
// landed on the server". The two are within one zlib block of each
// other in practice and the distinction does not matter for sizing.
func (o *packStreamObserver) ObjectsSent() int64 {
	return o.objectsSent.Load()
}

// TotalObjects returns the object count from the pack header, or 0 if
// the header has not been parsed yet (or was malformed).
func (o *packStreamObserver) TotalObjects() int64 {
	return o.totalObjects.Load()
}

// ScannerError returns the first error the Scanner produced, if any.
// A non-nil error means observation stopped early — counters above
// will not advance further. Useful for debugging; non-fatal for the
// upload itself, which is driven by Read on the tee.
func (o *packStreamObserver) ScannerError() error {
	if p := o.scannerErr.Load(); p != nil {
		return *p
	}
	return nil
}

func (o *packStreamObserver) consume(pr *io.PipeReader) {
	defer close(o.done)
	defer pr.Close()

	s := packfile.NewScanner(pr)
	for s.Scan() {
		d := s.Data()
		switch d.Section {
		case packfile.HeaderSection:
			if h, ok := d.Value().(packfile.Header); ok {
				o.totalObjects.Store(int64(h.ObjectsQty))
				o.headerOnce.Do(func() { close(o.headerReady) })
			}
		case packfile.ObjectSection:
			o.objectsSent.Add(1)
		case packfile.FooterSection:
			// Footer (pack checksum) carries no per-object signal; the
			// final ObjectSection has already incremented objectsSent.
		}
	}
	if err := s.Error(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		o.scannerErr.Store(&err)
	}
	// Ensure HeaderReady is closed even on a malformed pack so callers
	// waiting on it don't block forever.
	o.headerOnce.Do(func() { close(o.headerReady) })
}
