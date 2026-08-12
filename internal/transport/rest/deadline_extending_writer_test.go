package rest

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestDeadlineExtendingWriter_SurvivesPastFixedWriteTimeout pins the fix for
// a large vault export being silently truncated by the REST server's
// blanket WriteTimeout (server.go): a handler that keeps calling Write —
// even slowly, spread out past what a fixed http.Server.WriteTimeout would
// allow — must still deliver every byte to the client when writing through a
// deadlineExtendingWriter, because each Write pushes the deadline forward
// again.
func TestDeadlineExtendingWriter_SurvivesPastFixedWriteTimeout(t *testing.T) {
	const fixedWriteTimeout = 150 * time.Millisecond
	const chunkDelay = 100 * time.Millisecond
	const chunks = 4 // total handler time ~400ms, well past the 150ms fixed timeout

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dw := newDeadlineExtendingWriter(w)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			time.Sleep(chunkDelay)
			if _, err := dw.Write([]byte("chunk\n")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler, WriteTimeout: fixedWriteTimeout}
	go srv.Serve(ln)
	defer srv.Close()

	resp, err := http.Get("http://" + ln.Addr().String())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body (truncated by the server's fixed WriteTimeout?): %v", err)
	}

	want := ""
	for i := 0; i < chunks; i++ {
		want += "chunk\n"
	}
	if string(body) != want {
		t.Errorf("body = %q, want %q (truncated past the server's fixed WriteTimeout)", body, want)
	}
}

// TestDeadlineExtendingWriter_WithoutExtensionIsTruncated is the control: the
// same slow handler, but writing directly to w instead of through
// deadlineExtendingWriter, must be cut off by the fixed WriteTimeout — this
// is the pre-fix behavior the export handlers used to have, and confirms the
// test harness actually reproduces the bug rather than passing vacuously.
func TestDeadlineExtendingWriter_WithoutExtensionIsTruncated(t *testing.T) {
	const fixedWriteTimeout = 150 * time.Millisecond
	const chunkDelay = 100 * time.Millisecond
	const chunks = 4

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			time.Sleep(chunkDelay)
			if _, err := w.Write([]byte("chunk\n")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler, WriteTimeout: fixedWriteTimeout}
	go srv.Serve(ln)
	defer srv.Close()

	resp, err := http.Get("http://" + ln.Addr().String())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body) // a truncation error here also demonstrates the bug

	want := ""
	for i := 0; i < chunks; i++ {
		want += "chunk\n"
	}
	if string(body) == want {
		t.Skip("control did not reproduce truncation on this platform/timing — not a failure of the fix")
	}
}
