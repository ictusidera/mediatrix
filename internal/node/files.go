package node

import (
	"context"
	"errors"
	"io"

	"github.com/ictusidera/mediatrix/internal/model"
	"github.com/libp2p/go-libp2p/core/network"
)

type fileRequest struct {
	Key       string `json:"key"`
	TimeoutMS int64  `json:"timeout_ms"`
}
type fileHeader struct {
	Key   string       `json:"key,omitempty"`
	Size  int64        `json:"size,omitempty"`
	Error *model.Error `json:"error,omitempty"`
}

func (n *Node) Upload(ctx context.Context, r io.Reader) (model.FileInfo, error) {
	if !n.enter() {
		return model.FileInfo{}, model.Err("busy", "node concurrency limit reached")
	}
	defer n.leave()
	ctx, cancel := n.bounded(ctx, 0)
	defer cancel()
	info, err := n.store.Put(ctx, r, "")
	return info, storageError(err)
}
func (n *Node) SetFileAccess(ctx context.Context, key string, peers []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := model.FileDigest(key); err != nil {
		return model.Err("invalid", err.Error())
	}
	if err := n.validateACL(peers); err != nil {
		return err
	}
	if err := n.store.SetFileAccess(key, peers); err != nil {
		return storageError(err)
	}
	n.scheduleAnnounce()
	return nil
}
func (n *Node) OpenFile(key string) (io.ReadCloser, model.FileInfo, error) {
	r, info, err := n.store.OpenFile(key)
	return r, info, storageError(err)
}
func (n *Node) Fetch(ctx context.Context, req model.FetchRequest) (model.FileInfo, error) {
	if _, err := model.FileDigest(req.Key); err != nil {
		return model.FileInfo{}, model.Err("invalid", err.Error())
	}
	if req.TimeoutMS < 0 {
		return model.FileInfo{}, model.Err("invalid", "timeout_ms must be nonnegative")
	}
	if !n.enter() {
		return model.FileInfo{}, model.Err("busy", "node concurrency limit reached")
	}
	defer n.leave()
	ctx, cancel := n.bounded(ctx, req.TimeoutMS)
	defer cancel()
	stop := context.AfterFunc(n.ctx, cancel)
	defer stop()
	if _, ok := n.store.GetFile(req.Key); ok {
		f, info, err := n.store.OpenFile(req.Key)
		if err == nil {
			_ = f.Close()
		}
		return info, storageError(err)
	}
	for _, p := range n.providers(ctx, req.Key) {
		s, err := n.host.NewStream(network.WithAllowLimitedConn(ctx, "mediatrix"), p.ID, fileProtocol)
		if err != nil {
			continue
		}
		unbind := bindStream(ctx, s)
		err = writeFrame(s, fileRequest{Key: req.Key, TimeoutMS: req.TimeoutMS}, n.cfg.Limits.MaxJSONBytes)
		var h fileHeader
		if err == nil {
			err = readFrame(s, &h, n.cfg.Limits.MaxJSONBytes)
		}
		if err == nil && h.Error != nil {
			err = h.Error
		}
		if err == nil && (h.Key != req.Key || h.Size < 0 || h.Size > n.cfg.Limits.MaxFileBytes) {
			err = model.Err("integrity", "invalid file metadata")
		}
		var info model.FileInfo
		if err == nil {
			// Include an extra byte in the bounded reader: too many bytes fail size
			// validation before publication. SHA256 is verified by store.Put.
			exact := &exactReader{r: s, remaining: h.Size}
			info, err = n.store.Put(ctx, exact, req.Key)
		}
		unbind()
		_ = s.Close()
		if err != nil {
			return model.FileInfo{}, storageError(err)
		}
		return info, nil
	}
	if ctx.Err() != nil {
		return model.FileInfo{}, timeoutError(ctx.Err(), false)
	}
	return model.FileInfo{}, model.Err("not_found", "no reachable authorized provider")
}

// exactReader requires precisely the announced byte count and EOF. It rejects
// trailing bytes before Store can commit the hash-addressed file.
type exactReader struct {
	r         io.Reader
	remaining int64
	finished  bool
}

func (r *exactReader) Read(p []byte) (int, error) {
	if r.finished {
		return 0, io.EOF
	}
	if r.remaining == 0 {
		var b [1]byte
		n, e := r.r.Read(b[:])
		r.finished = true
		if n != 0 {
			return 0, model.Err("integrity", "file exceeds announced size")
		}
		if e == io.EOF {
			return 0, io.EOF
		}
		if e == nil {
			return 0, io.ErrNoProgress
		}
		return 0, e
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, e := r.r.Read(p)
	r.remaining -= int64(n)
	if errors.Is(e, io.EOF) && r.remaining > 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, e
}
func (n *Node) handleFile(s network.Stream) {
	if !n.startInbound(s) {
		return
	}
	defer n.finishInbound()
	defer s.Close()
	var req fileRequest
	if err := readFrame(s, &req, n.cfg.Limits.MaxJSONBytes); err != nil {
		_ = s.Reset()
		return
	}
	if _, err := model.FileDigest(req.Key); err != nil || !n.available(req.Key, s.Conn().RemotePeer().String()) {
		_ = writeFrame(s, fileHeader{Error: model.Err("not_found", "file unavailable")}, n.cfg.Limits.MaxJSONBytes)
		return
	}
	ctx, cancel := n.bounded(n.ctx, req.TimeoutMS)
	defer cancel()
	unbind := bindStream(ctx, s)
	defer unbind()
	f, info, err := n.store.OpenFile(req.Key)
	if err != nil {
		_ = writeFrame(s, fileHeader{Error: model.Err("unavailable", "file unavailable")}, n.cfg.Limits.MaxJSONBytes)
		return
	}
	defer f.Close()
	if writeFrame(s, fileHeader{Key: req.Key, Size: info.Size}, n.cfg.Limits.MaxJSONBytes) != nil {
		return
	}
	if _, err = io.CopyN(s, f, info.Size); err != nil {
		_ = s.Reset()
		return
	}
	_ = s.CloseWrite()
}
