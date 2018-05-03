package broker

import (
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/gogo/protobuf/proto"
	"github.com/gorilla/schema"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/trace"
	"google.golang.org/grpc"

	pb "github.com/LiveRamp/gazette/pkg/protocol"
)

type HTTPAPI struct {
	broker  *Router
	decoder *schema.Decoder
}

func NewHTTPAPI(broker *Router) *HTTPAPI {
	var decoder = schema.NewDecoder()
	decoder.IgnoreUnknownKeys(false)

	return &HTTPAPI{
		broker:  broker,
		decoder: decoder,
	}
}

func (h *HTTPAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET", "HEAD":
		h.serveRead(w, r)
	case "PUT":
		h.serveWrite(w, r)
	default:
		http.Error(w, fmt.Sprintf("unknown method: %s", r.Method), http.StatusBadRequest)
	}
}

func (h *HTTPAPI) serveRead(w http.ResponseWriter, r *http.Request) {
	var req, err = h.parseReadRequest(r)

	if err != nil {
		if tr, ok := trace.FromContext(r.Context()); ok {
			tr.LazyPrintf("parsing request: %v", err)
			tr.SetError()
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var client pb.Broker_ReadClient
	var conn *grpc.ClientConn
	var resolution resolution
	var resp = new(pb.ReadResponse)

	resolution, resp.Status = h.broker.resolve(req.Journal, false, true)

	if resp.Status != pb.Status_OK {
		h.writeReadResponse(w, r, resp)
		return
	}

	if conn, err = h.broker.peerConn(resolution.broker); err == nil {
		if client, err = pb.NewBrokerClient(conn).Read(r.Context(), req); err == nil {
			err = client.RecvMsg(resp)
		}
	}

	if err != nil {
		if tr, ok := trace.FromContext(r.Context()); ok {
			tr.LazyPrintf("evaluating request: %v", err)
			tr.SetError()
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.writeReadResponse(w, r, resp)

	for {
		if err = client.RecvMsg(resp); err == io.EOF {
			break // Done.
		} else if err != nil {
			log.WithField("err", err).Warn("httpapi: failed to proxy Read response")
			break // Done.
		}

		if _, err = w.Write(resp.Content); err != nil {
			log.WithField("err", err).Warn("httpapi: failed to forward Read response")
			break
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func (h *HTTPAPI) serveWrite(w http.ResponseWriter, r *http.Request) {
	var req, err = h.parseAppendRequest(r)

	if err != nil {
		if tr, ok := trace.FromContext(r.Context()); ok {
			tr.LazyPrintf("parsing request: %v", err)
			tr.SetError()
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var client pb.Broker_AppendClient
	var conn *grpc.ClientConn
	var resolution resolution
	var resp = new(pb.AppendResponse)

	resolution, resp.Status = h.broker.resolve(req.Journal, true, true)

	if resp.Status != pb.Status_OK {
		h.writeAppendResponse(w, r, resp)
		return
	}

	if conn, err = h.broker.peerConn(resolution.broker); err == nil {
		if client, err = pb.NewBrokerClient(conn).Append(r.Context()); err == nil {
			err = client.SendMsg(req)
			*req = pb.AppendRequest{} // Clear metadata: hereafter, only Content is sent.
		}
	}

	var buffer = chunkBufferPool.Get().([]byte)
	defer chunkBufferPool.Put(buffer)

	// Proxy content chunks from the http.Request through the Broker_AppendClient.
	var n int
	for done := false; !done && err == nil; {
		if n, err = r.Body.Read(buffer); err == io.EOF {
			done, err = true, nil
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			continue
		}

		req.Content = buffer[:n]
		if err = client.Send(req); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}

	if err == nil {
		if resp, err = client.CloseAndRecv(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}

	if err != nil {
		if tr, ok := trace.FromContext(r.Context()); ok {
			tr.LazyPrintf("evaluating request: %v", err)
			tr.SetError()
		}
		return
	}
	h.writeAppendResponse(w, r, resp)
}

func (h *HTTPAPI) parseAppendRequest(r *http.Request) (*pb.AppendRequest, error) {
	var schema struct{}

	var err error
	if err = r.ParseForm(); err == nil {
		err = h.decoder.Decode(&schema, r.Form)
	}

	var req = &pb.AppendRequest{
		Journal: pb.Journal(r.URL.Path[1:]),
	}
	if err == nil {
		err = req.Validate()
	}
	return req, err
}

func (h *HTTPAPI) parseReadRequest(r *http.Request) (*pb.ReadRequest, error) {
	var schema struct {
		Offset int64
		Block  bool
	}

	var err error
	if err = r.ParseForm(); err == nil {
		err = h.decoder.Decode(&schema, r.Form)
	}

	var req = &pb.ReadRequest{
		Journal:      pb.Journal(r.URL.Path[1:]),
		Offset:       schema.Offset,
		Block:        schema.Block,
		MetadataOnly: r.Method == "HEAD",
	}
	if err == nil {
		err = req.Validate()
	}
	return req, err
}

func (h *HTTPAPI) writeAppendResponse(w http.ResponseWriter, r *http.Request, resp *pb.AppendResponse) {
	if resp.Route != nil {
		w.Header().Set(RouteTokenHeader, proto.CompactTextString(resp.Route))
	}
	if resp.FirstOffset != 0 {
		w.Header().Add(FirstOffsetHeader, strconv.FormatInt(resp.FirstOffset, 10))
	}
	if resp.LastOffset != 0 {
		w.Header().Add(LastOffsetHeader, strconv.FormatInt(resp.LastOffset, 10))
	}
	if resp.WriteHead != 0 {
		w.Header().Add(WriteHeadHeader, strconv.FormatInt(resp.WriteHead, 10))
	}

	switch resp.Status {
	case pb.Status_OK:
		w.WriteHeader(http.StatusNoContent) // 204.
	case pb.Status_JOURNAL_NOT_FOUND:
		w.WriteHeader(http.StatusNotFound) // 404.
	case pb.Status_REPLICATION_FAILED:
		http.Error(w, resp.Status.String(), http.StatusServiceUnavailable) // 503.
	default:
		http.Error(w, resp.Status.String(), http.StatusInternalServerError) // 500.
	}
}

func (h *HTTPAPI) writeReadResponse(w http.ResponseWriter, r *http.Request, resp *pb.ReadResponse) {
	if resp.Route != nil {
		w.Header().Set(RouteTokenHeader, proto.CompactTextString(resp.Route))
	}
	if resp.Fragment != nil {
		w.Header().Add(FragmentNameHeader, resp.Fragment.ContentName())

		if !resp.Fragment.ModTime.IsZero() {
			w.Header().Add(FragmentLastModifiedHeader, resp.Fragment.ModTime.Format(http.TimeFormat))
		}
		if resp.FragmentUrl != "" {
			w.Header().Add(FragmentLocationHeader, resp.FragmentUrl)
		}
	}
	if resp.WriteHead != 0 {
		w.Header().Add(WriteHeadHeader, strconv.FormatInt(resp.WriteHead, 10))
	}

	switch resp.Status {
	case pb.Status_OK:
		w.WriteHeader(http.StatusPartialContent) // 206.
	case pb.Status_JOURNAL_NOT_FOUND:
		http.Error(w, resp.Status.String(), http.StatusNotFound) // 404.
	case pb.Status_NO_JOURNAL_BROKERS:
		http.Error(w, resp.Status.String(), http.StatusServiceUnavailable) // 503.
	case pb.Status_OFFSET_NOT_YET_AVAILABLE:
		http.Error(w, resp.Status.String(), http.StatusRequestedRangeNotSatisfiable) // 416.
	default:
		http.Error(w, resp.Status.String(), http.StatusInternalServerError) // 500.
	}
}

const (
	FragmentLastModifiedHeader = "X-Fragment-Last-Modified"
	FragmentLocationHeader     = "X-Fragment-Location"
	FragmentNameHeader         = "X-Fragment-Name"
	RouteTokenHeader           = "X-Route-Token"
	WriteHeadHeader            = "X-Write-Head"
	FirstOffsetHeader          = "X-First-Offset"
	LastOffsetHeader           = "X-Last-Offset"
)
