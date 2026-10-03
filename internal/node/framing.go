package node

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
)

func writeFrame(w io.Writer, v any, max int64) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if int64(len(b)) > max {
		return fmt.Errorf("frame too large")
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(b)))
	if err = writeAll(w, h[:]); err != nil {
		return err
	}
	return writeAll(w, b)
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func readFrame(r io.Reader, v any, max int64) error {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return err
	}
	size := int64(binary.BigEndian.Uint32(h[:]))
	if size < 2 || size > max {
		return fmt.Errorf("invalid frame length")
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
func bindStream(ctx context.Context, s network.Stream) func() {
	if d, ok := ctx.Deadline(); ok {
		_ = s.SetDeadline(d)
	}
	stop := context.AfterFunc(ctx, func() { _ = s.Reset() })
	return func() { stop() }
}
func (n *Node) bounded(ctx context.Context, ms int64) (context.Context, context.CancelFunc) {
	d := time.Duration(n.cfg.Limits.TimeoutSeconds) * time.Second
	if ms > 0 && ms < d.Milliseconds() {
		d = time.Duration(ms) * time.Millisecond
	}
	return context.WithTimeout(ctx, d)
}
