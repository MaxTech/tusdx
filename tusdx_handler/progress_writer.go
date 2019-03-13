package tusdx_handler

import "sync/atomic"

type progressWriter struct {
    Offset int64
}

func (w *progressWriter) Write(b []byte) (int, error) {
    atomic.AddInt64(&w.Offset, int64(len(b)))
    return len(b), nil
}
