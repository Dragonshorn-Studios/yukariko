package runner

// limitedWriter captures up to limit bytes per stream and records the full
// pre-truncation size, so summaries can say both "here is a bounded excerpt"
// and "the command emitted N bytes in total".
type limitedWriter struct {
	buf   []byte
	limit int64
	size  int64
}

func newLimitedWriter(limit int64) *limitedWriter {
	return &limitedWriter{buf: make([]byte, 0, min64(limit, 1<<16)), limit: limit}
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.size += int64(n)
	if remaining := w.limit - int64(len(w.buf)); remaining > 0 {
		take := n
		if int64(take) > remaining {
			take = int(remaining)
		}
		w.buf = append(w.buf, p[:take]...)
	}
	return n, nil
}

func (w *limitedWriter) snapshot() (data []byte, truncated bool, total int64) {
	return w.buf, w.size > w.limit, w.size
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
