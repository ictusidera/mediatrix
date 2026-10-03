package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/ictusidera/mediatrix/internal/model"
	"github.com/libp2p/go-libp2p/core/network"
)

func (n *Node) Call(ctx context.Context, req model.CallRequest) (model.CallResponse, error) {
	response := model.CallResponse{RequestID: req.RequestID}
	if err := validateCall(req); err != nil {
		return response, model.Err("invalid", err.Error())
	}
	if req.RequestID == "" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return response, err
		}
		req.RequestID = hex.EncodeToString(id[:])
		response.RequestID = req.RequestID
	}
	if !n.enter() {
		return response, model.Err("busy", "node concurrency limit reached")
	}
	defer n.leave()
	ctx, cancel := n.bounded(ctx, req.TimeoutMS)
	defer cancel()
	stop := context.AfterFunc(n.ctx, cancel)
	defer stop()
	if _, ok := n.store.GetService(req.Service); ok {
		return n.invoke(ctx, n.host.ID().String(), req)
	}
	providers := n.providers(ctx, req.Service)
	for _, p := range providers {
		if err := ctx.Err(); err != nil {
			return response, timeoutError(err, false)
		}
		s, err := n.host.NewStream(network.WithAllowLimitedConn(ctx, "mediatrix"), p.ID, rpcProtocol)
		if err != nil {
			continue
		}
		d, _ := ctx.Deadline()
		req.TimeoutMS = max(1, time.Until(d).Milliseconds())
		b, e := json.Marshal(req)
		if e != nil || int64(len(b)) > n.cfg.Limits.MaxJSONBytes {
			_ = s.Reset()
			return response, model.Err("too_large", "RPC request exceeds frame limit")
		}
		unbind := bindStream(ctx, s)
		// From this point the peer may have received/started the request. No retry.
		err = writeFrame(s, req, n.cfg.Limits.MaxJSONBytes)
		if err == nil {
			err = readFrame(s, &response, n.cfg.Limits.MaxJSONBytes)
		}
		unbind()
		_ = s.Close()
		if err != nil {
			return model.CallResponse{RequestID: req.RequestID}, timeoutError(ctx.Err(), true)
		}
		if response.RequestID != req.RequestID || (len(response.Result) == 0) == (response.Error == nil) {
			return model.CallResponse{RequestID: req.RequestID}, &model.Error{Code: "outcome_unknown", Message: "invalid peer response; execution may have occurred", OutcomeUnknown: true}
		}
		return response, nil
	}
	if ctx.Err() != nil {
		return response, timeoutError(ctx.Err(), false)
	}
	return response, model.Err("not_found", "no reachable authorized provider")
}
func timeoutError(err error, sent bool) *model.Error {
	code, msg := "unavailable", "provider unavailable"
	if errors.Is(err, context.DeadlineExceeded) {
		code, msg = "timeout", "operation deadline exceeded"
	} else if errors.Is(err, context.Canceled) {
		code, msg = "canceled", "operation canceled"
	} else if sent {
		code, msg = "outcome_unknown", "response lost; execution may have occurred"
	}
	return &model.Error{Code: code, Message: msg, OutcomeUnknown: sent}
}
func (n *Node) handleRPC(s network.Stream) {
	if !n.startInbound(s) {
		return
	}
	defer n.finishInbound()
	defer s.Close()
	var req model.CallRequest
	if err := readFrame(s, &req, n.cfg.Limits.MaxJSONBytes); err != nil {
		_ = s.Reset()
		return
	}
	if err := validateCall(req); err != nil {
		_ = writeFrame(s, model.CallResponse{RequestID: req.RequestID, Error: model.Err("invalid", "invalid request")}, n.cfg.Limits.MaxJSONBytes)
		return
	}
	ctx, cancel := n.bounded(n.ctx, req.TimeoutMS)
	defer cancel()
	stop := bindStream(ctx, s)
	defer stop()
	// The caller keeps its write side open. EOF/reset/extra bytes cancel work.
	go func() { var b [1]byte; _, _ = s.Read(b[:]); cancel() }()
	start := time.Now()
	resp, err := n.invoke(ctx, s.Conn().RemotePeer().String(), req)
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) {
			resp.Error = typed
		} else {
			resp.Error = model.Err("internal", "handler failed")
		}
		resp.Result = nil
	}
	n.log.Info("rpc finished", "peer", s.Conn().RemotePeer().String(), "request_id", req.RequestID, "duration_ms", time.Since(start).Milliseconds(), "success", err == nil && resp.Error == nil)
	if err := writeFrame(s, resp, n.cfg.Limits.MaxJSONBytes); err != nil {
		_ = s.Reset()
	}
}
