package bootstrap

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
)

// buildSyntheticPack writes a pack containing n distinct blobs and a
// single commit referencing the last blob via a tree. Returns the raw
// pack bytes plus the count of objects encoded (blobs + tree + commit).
// Used by observer tests so we can drive the Scanner against a real
// PACK + zlib stream rather than relying on a fixed fixture.
func buildSyntheticPack(t *testing.T, blobCount int) ([]byte, int) {
	t.Helper()
	store := memory.NewStorage()

	hashes := make([]plumbing.Hash, 0, blobCount+2)

	for i := range blobCount {
		obj := store.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		w, err := obj.Writer()
		if err != nil {
			t.Fatalf("blob writer: %v", err)
		}
		// Each blob carries unique content so deltas don't collapse them.
		content := []byte(t.Name() + "/blob/" + string(rune('a'+i%26)) + "\n")
		if _, err := w.Write(content); err != nil {
			t.Fatalf("blob write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("blob close: %v", err)
		}
		obj.SetSize(int64(len(content)))
		h, err := store.SetEncodedObject(obj)
		if err != nil {
			t.Fatalf("blob set: %v", err)
		}
		hashes = append(hashes, h)
	}

	tree := &object.Tree{
		Entries: []object.TreeEntry{
			{Name: "blob", Mode: 0o100644, Hash: hashes[len(hashes)-1]},
		},
	}
	treeObj := store.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		t.Fatalf("tree encode: %v", err)
	}
	treeHash, err := store.SetEncodedObject(treeObj)
	if err != nil {
		t.Fatalf("tree set: %v", err)
	}
	hashes = append(hashes, treeHash)

	commit := &object.Commit{
		Author:    object.Signature{Name: "Test", Email: "test@example.com", When: time.Unix(0, 0)},
		Committer: object.Signature{Name: "Test", Email: "test@example.com", When: time.Unix(0, 0)},
		Message:   "test",
		TreeHash:  treeHash,
	}
	commitObj := store.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		t.Fatalf("commit encode: %v", err)
	}
	commitHash, err := store.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatalf("commit set: %v", err)
	}
	hashes = append(hashes, commitHash)

	var buf bytes.Buffer
	enc := packfile.NewEncoder(&buf, store, false)
	if _, err := enc.Encode(hashes, 0); err != nil {
		t.Fatalf("encode pack: %v", err)
	}
	return buf.Bytes(), len(hashes)
}

func TestPackStreamObserverCountsObjects(t *testing.T) {
	t.Parallel()
	const blobCount = 20
	pack, total := buildSyntheticPack(t, blobCount)

	o := newPackStreamObserver(io.NopCloser(bytes.NewReader(pack)))
	out, err := io.ReadAll(o)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(out, pack) {
		t.Fatalf("observer altered pack content (len got %d, want %d)", len(out), len(pack))
	}
	if err := o.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if scanErr := o.ScannerError(); scanErr != nil {
		t.Fatalf("unexpected scanner error: %v", scanErr)
	}
	if got := o.TotalObjects(); got != int64(total) {
		t.Errorf("TotalObjects() = %d, want %d", got, total)
	}
	if got := o.ObjectsSent(); got != int64(total) {
		t.Errorf("ObjectsSent() = %d, want %d after fully reading pack", got, total)
	}
	if got := o.Bytes(); got != int64(len(pack)) {
		t.Errorf("Bytes() = %d, want %d", got, len(pack))
	}
}

// A Scanner error mid-stream must not abort the upload: every byte handed in
// must still come back out of Read (the documented "non-fatal for the upload"
// contract), with the error recorded only for debugging. Previously the
// Scanner's deferred pipe close made the TeeReader's write fail and killed the
// push.
func TestPackStreamObserverScannerErrorDoesNotAbortUpload(t *testing.T) {
	t.Parallel()
	// Not a valid packfile — the Scanner fails parsing the header almost
	// immediately and closes its pipe reader while bytes are still flowing.
	payload := bytes.Repeat([]byte("definitely-not-a-packfile\n"), 2048)

	o := newPackStreamObserver(io.NopCloser(bytes.NewReader(payload)))
	out, err := io.ReadAll(o)
	if err != nil {
		t.Fatalf("upload Read aborted by a non-fatal observer error: %v", err)
	}
	if !bytes.Equal(out, payload) {
		t.Fatalf("observer dropped bytes: got %d, want %d", len(out), len(payload))
	}
	if err := o.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if o.ScannerError() == nil {
		t.Fatal("expected a scanner error to be recorded for non-pack input")
	}
}

// TestPackStreamObserverHeaderReadyEarly verifies that the header is
// observed before all bytes have been pulled — important for callers
// that want to make subdivision decisions partway through the upload.
func TestPackStreamObserverHeaderReadyEarly(t *testing.T) {
	t.Parallel()
	pack, total := buildSyntheticPack(t, 50)

	o := newPackStreamObserver(io.NopCloser(bytes.NewReader(pack)))
	defer func() { _ = o.Close() }()

	// Read just enough bytes for the 12-byte header (a few extra to give
	// the Scanner room to actually parse it). The observer must surface
	// TotalObjects shortly after.
	buf := make([]byte, 64)
	n, err := io.ReadFull(o, buf)
	if err != nil {
		t.Fatalf("read header: %v (n=%d)", err, n)
	}

	select {
	case <-o.HeaderReady():
	case <-time.After(time.Second):
		t.Fatal("HeaderReady never fired after 64 bytes were read")
	}
	if got := o.TotalObjects(); got != int64(total) {
		t.Errorf("TotalObjects() = %d after header, want %d", got, total)
	}

	// Drain the rest so Close drains cleanly.
	if _, err := io.Copy(io.Discard, o); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

// TestPackStreamObserverAbortStopsRead verifies that once the aborter
// returns true the observer surfaces ErrPackUploadAborted on every
// subsequent Read, even though the underlying source still has bytes.
// This is the contract the bootstrap loop relies on to short-circuit
// a doomed upload.
func TestPackStreamObserverAbortStopsRead(t *testing.T) {
	t.Parallel()
	pack, _ := buildSyntheticPack(t, 80)

	o := newPackStreamObserver(io.NopCloser(bytes.NewReader(pack)))
	defer func() { _ = o.Close() }()

	// Trigger after the first 32 bytes pass through.
	o.SetAborter(func(bytesSent, _, _ int64, _ bool) bool {
		return bytesSent >= 32
	})

	buf := make([]byte, 64)
	for i := range 4 {
		n, err := o.Read(buf)
		if errors.Is(err, ErrPackUploadAborted) {
			if !o.Aborted() {
				t.Errorf("Aborted() must be true after the sentinel error fires")
			}
			return
		}
		if err != nil {
			t.Fatalf("read %d returned %v (n=%d)", i, err, n)
		}
	}
	t.Fatal("expected ErrPackUploadAborted within 4 reads")
}

// TestPackStreamObserverCloseBeforeFullRead ensures Close is safe even
// when the caller stops reading early (e.g., HTTP body cancelled).
func TestPackStreamObserverCloseBeforeFullRead(t *testing.T) {
	t.Parallel()
	pack, _ := buildSyntheticPack(t, 30)

	o := newPackStreamObserver(io.NopCloser(bytes.NewReader(pack)))
	// Read a small prefix, then close.
	if _, err := io.CopyN(io.Discard, o, 32); err != nil {
		t.Fatalf("partial read: %v", err)
	}
	if err := o.Close(); err != nil {
		t.Fatalf("close after partial read: %v", err)
	}
}

// Readers may return their final bytes together with EOF. The byte guard must
// inspect those bytes before treating the upload as complete.
type finalBytesReader struct{ data []byte }

func (r *finalBytesReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}
func TestPackStreamObserverChecksFinalBytesWithEOF(t *testing.T) {
	t.Parallel()
	o := newPackStreamObserver(io.NopCloser(&finalBytesReader{data: []byte("over limit")}))
	defer o.Close()
	o.SetAborter(func(n, _, _ int64, _ bool) bool { return n > 4 })
	_, err := io.Copy(io.Discard, o)
	if !errors.Is(err, ErrPackUploadAborted) || !o.Aborted() {
		t.Fatalf("final bytes bypassed ceiling: %v", err)
	}
}

func TestPackObservationDistinguishesInterruptedUploads(t *testing.T) {
	t.Parallel()
	valid, _ := buildSyntheticPack(t, 10)
	for _, tc := range []struct {
		name                                         string
		data                                         []byte
		partial, abort, wantFailure, wantInterrupted bool
	}{
		{name: "complete valid pack", data: valid},
		{name: "server stops upload", data: valid, partial: true, wantInterrupted: true},
		{name: "client aborts upload", data: valid, partial: true, abort: true, wantInterrupted: true},
		{name: "source is truncated", data: valid[:64], wantFailure: true},
		{name: "malformed before close", data: bytes.Repeat([]byte("x"), 128), partial: true, wantFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := newPackStreamObserver(io.NopCloser(bytes.NewReader(tc.data)))
			if tc.abort {
				o.SetAborter(func(n, _, _ int64, _ bool) bool { return n >= 64 })
			}
			if tc.partial {
				_, err := o.Read(make([]byte, 64))
				if tc.abort && !errors.Is(err, ErrPackUploadAborted) {
					t.Fatalf("expected abort: %v", err)
				}
			} else {
				if _, err := io.Copy(io.Discard, o); err != nil {
					t.Fatal(err)
				}
			}
			if err := o.Close(); err != nil {
				t.Fatal(err)
			}
			if (o.ScannerError() != nil) != tc.wantFailure || o.interrupted.Load() != tc.wantInterrupted {
				t.Fatalf("error=%v interrupted=%v; want failure=%v interrupted=%v", o.ScannerError(), o.interrupted.Load(), tc.wantFailure, tc.wantInterrupted)
			}
		})
	}
}

func TestPackStreamObserverEOFUsesActualBytesDespiteScannerLag(t *testing.T) {
	t.Parallel()
	const size = 8 << 20
	for _, separateEOF := range []bool{false, true} {
		for _, budget := range []int64{16 << 20, 4 << 20} {
			name := fmt.Sprintf("separateEOF=%v/budget=%d", separateEOF, budget)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				// Freeze a lagging scanner state independently of goroutine scheduling.
				// Its projection is 800 MiB, but the final stream size is 8 MiB.
				var reader io.Reader = &finalBytesReader{data: bytes.Repeat([]byte("x"), size)}
				o := newPackStreamObserver(io.NopCloser(reader))
				// Bypass only the scanner tee to freeze its scheduling state. Retain
				// the real constructor and shutdown lifecycle.
				o.tee = reader
				defer o.Close()
				if separateEOF {
					o.tee = bytes.NewReader(nil)
					o.bytes.Store(size)
				}
				o.totalObjects.Store(100)
				o.objectsSent.Store(1)
				o.SetAborter(func(n, objects, total int64, atEOF bool) bool {
					return shouldAbortPush(n, objects, total, budget, atEOF)
				})
				_, err := o.Read(make([]byte, size))
				if budget > size {
					if !errors.Is(err, io.EOF) || o.Aborted() {
						t.Fatalf("completed pack falsely projected: %v", err)
					}
				} else {
					if !errors.Is(err, ErrPackUploadAborted) {
						t.Fatalf("EOF bypassed byte guard: %v", err)
					}
					snapshot := o.abortCounters.Load()
					if snapshot == nil || snapshot.objects != 1 || snapshot.total != 100 || snapshot.bytes != size {
						t.Fatalf("diagnostic counters were changed: %+v", snapshot)
					}
				}
			})
		}
	}
}

// sourceFailureReader can report a failure with its final bytes or on a
// subsequent read, as network readers are permitted to do.
type sourceFailureReader struct {
	data     []byte
	failure  error
	separate bool
}

func (r *sourceFailureReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 && (!r.separate || n == 0) {
		return n, r.failure
	}
	return n, nil
}

func TestPackObservationPreservesSourceReadFailure(t *testing.T) {
	t.Parallel()
	valid, _ := buildSyntheticPack(t, 10)
	for _, failure := range []error{io.ErrUnexpectedEOF, io.ErrClosedPipe, fmt.Errorf("source body: %w", io.ErrUnexpectedEOF), errors.New("source connection reset")} {
		for _, separate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/separate=%v", failure, separate), func(t *testing.T) {
				t.Parallel()
				o := newPackStreamObserver(io.NopCloser(&sourceFailureReader{data: valid[:64], failure: failure, separate: separate}))
				_, err := io.Copy(io.Discard, o)
				if !errors.Is(err, failure) {
					t.Fatalf("source error lost during upload: %v", err)
				}
				if err := o.Close(); err != nil {
					t.Fatal(err)
				}
				if !errors.Is(o.ScannerError(), failure) || o.interrupted.Load() {
					t.Fatalf("source failure mislabeled: scanner=%v interrupted=%v", o.ScannerError(), o.interrupted.Load())
				}
			})
		}
	}
}

func TestPackEOFMarginBandAndHardCeiling(t *testing.T) {
	t.Parallel()
	const budget = 1 << 20
	for _, size := range []int{997232, budget, budget + 1} {
		for _, separate := range []bool{false, true} {
			t.Run(fmt.Sprintf("size=%d/separate=%v", size, separate), func(t *testing.T) {
				t.Parallel()
				o := newPackStreamObserver(io.NopCloser(&finalBytesReader{data: bytes.Repeat([]byte("x"), size)}))
				defer o.Close()
				if separate {
					// Final EOF after previously delivered bytes, with no scheduling race.
					o.bytes.Store(int64(size))
					o.tee = bytes.NewReader(nil)
				}
				o.SetAborter(func(n, objects, total int64, eof bool) bool { return shouldAbortPush(n, objects, total, budget, eof) })
				_, err := o.Read(make([]byte, size))
				if size <= budget {
					if !errors.Is(err, io.EOF) || o.Aborted() {
						t.Fatalf("complete pack inside ceiling rejected: %v", err)
					}
				} else if !errors.Is(err, ErrPackUploadAborted) {
					t.Fatalf("hard ceiling bypassed: %v", err)
				}
			})
		}
	}
}
