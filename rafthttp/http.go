// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rafthttp

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"path"

	"github.com/algoadv/etcd/Godeps/_workspace/src/golang.org/x/net/context"
	pioutil "github.com/algoadv/etcd/pkg/ioutil"
	"github.com/algoadv/etcd/pkg/types"
	"github.com/algoadv/etcd/raft/raftpb"
	"github.com/algoadv/etcd/version"
)

const (
	ConnReadLimitByte = 64 * 1024
)

var (
	RaftPrefix         = "/raft"
	ProbingPrefix      = path.Join(RaftPrefix, "probing")
	RaftStreamPrefix   = path.Join(RaftPrefix, "stream")
	RaftSnapshotPrefix = path.Join(RaftPrefix, "snapshot")

	errIncompatibleVersion = errors.New("incompatible version")
	errClusterIDMismatch   = errors.New("cluster ID mismatch")
)

func NewHandler(r Raft, cid types.ID) http.Handler {
	return &handler{
		r:   r,
		cid: cid,
	}
}

func newSnapshotHandler(r Raft, snapSaver SnapshotSaver, cid types.ID) http.Handler {
	return &snapshotHandler{
		r:         r,
		snapSaver: snapSaver,
		cid:       cid,
	}
}

type peerGetter interface {
	Get(id types.ID) Peer
}

func newStreamHandler(peerGetter peerGetter, r Raft, id, cid types.ID) http.Handler {
	return &streamHandler{
		peerGetter: peerGetter,
		r:          r,
		id:         id,
		cid:        cid,
	}
}

type writerToResponse interface {
	WriteTo(w http.ResponseWriter)
}

type handler struct {
	r   Raft
	cid types.ID
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("X-Etcd-Cluster-ID", h.cid.String())

	if err := checkClusterCompatibilityFromHeader(r.Header, h.cid); err != nil {
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	}

	// Limit the data size that could be read from the request body, which ensures that read from
	// connection will not time out accidentally due to possible block in underlying implementation.
	limitedr := pioutil.NewLimitedBufferReader(r.Body, ConnReadLimitByte)
	b, err := ioutil.ReadAll(limitedr)
	if err != nil {
		plog.Errorf("failed to read raft message (%v)", err)
		http.Error(w, "error reading raft message", http.StatusBadRequest)
		return
	}
	var m raftpb.Message
	if err := m.Unmarshal(b); err != nil {
		plog.Errorf("failed to unmarshal raft message (%v)", err)
		http.Error(w, "error unmarshaling raft message", http.StatusBadRequest)
		return
	}
	if err := h.r.Process(context.TODO(), m); err != nil {
		switch v := err.(type) {
		case writerToResponse:
			v.WriteTo(w)
		default:
			plog.Warningf("failed to process raft message (%v)", err)
			http.Error(w, "error processing raft message", http.StatusInternalServerError)
		}
		return
	}
	// Write StatusNoContet header after the message has been processed by
	// raft, which facilitates the client to report MsgSnap status.
	w.WriteHeader(http.StatusNoContent)
}

type snapshotHandler struct {
	r         Raft
	snapSaver SnapshotSaver
	cid       types.ID
}

// ServeHTTP serves HTTP request to receive and process snapshot message.
//
// If request sender dies without closing underlying TCP connection,
// the handler will keep waiting for the request body until TCP keepalive
// finds out that the connection is broken after several minutes.
// This is acceptable because
// 1. snapshot messages sent through other TCP connections could still be
// received and processed.
// 2. this case should happen rarely, so no further optimization is done.
func (h *snapshotHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("X-Etcd-Cluster-ID", h.cid.String())

	if err := checkClusterCompatibilityFromHeader(r.Header, h.cid); err != nil {
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	}

	dec := &messageDecoder{r: r.Body}
	m, err := dec.decode()
	if err != nil {
		msg := fmt.Sprintf("failed to decode raft message (%v)", err)
		plog.Errorf(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if m.Type != raftpb.MsgSnap {
		plog.Errorf("unexpected raft message type %s on snapshot path", m.Type)
		http.Error(w, "wrong raft message type", http.StatusBadRequest)
		return
	}

	// save snapshot
	if err := h.snapSaver.SaveFrom(r.Body, m.Snapshot.Metadata.Index); err != nil {
		msg := fmt.Sprintf("failed to save KV snapshot (%v)", err)
		plog.Error(msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	plog.Infof("received and saved snapshot [index: %d, from: %s] successfully", m.Snapshot.Metadata.Index, types.ID(m.From))

	if err := h.r.Process(context.TODO(), m); err != nil {
		switch v := err.(type) {
		// Process may return writerToResponse error when doing some
		// additional checks before calling raft.Node.Step.
		case writerToResponse:
			v.WriteTo(w)
		default:
			msg := fmt.Sprintf("failed to process raft message (%v)", err)
			plog.Warningf(msg)
			http.Error(w, msg, http.StatusInternalServerError)
		}
		return
	}
	// Write StatusNoContet header after the message has been processed by
	// raft, which facilitates the client to report MsgSnap status.
	w.WriteHeader(http.StatusNoContent)
}

type streamHandler struct {
	peerGetter peerGetter
	r          Raft
	id         types.ID
	cid        types.ID
}

func (h *streamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("X-Server-Version", version.Version)
	w.Header().Set("X-Etcd-Cluster-ID", h.cid.String())

	if err := checkClusterCompatibilityFromHeader(r.Header, h.cid); err != nil {
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	}

	var t streamType
	switch path.Dir(r.URL.Path) {
	// backward compatibility
	case RaftStreamPrefix:
		t = streamTypeMsgApp
	case path.Join(RaftStreamPrefix, string(streamTypeMsgApp)):
		t = streamTypeMsgAppV2
	case path.Join(RaftStreamPrefix, string(streamTypeMessage)):
		t = streamTypeMessage
	default:
		plog.Debugf("ignored unexpected streaming request path %s", r.URL.Path)
		http.Error(w, "invalid path", http.StatusNotFound)
		return
	}

	fromStr := path.Base(r.URL.Path)
	from, err := types.IDFromString(fromStr)
	if err != nil {
		plog.Errorf("failed to parse from %s into ID (%v)", fromStr, err)
		http.Error(w, "invalid from", http.StatusNotFound)
		return
	}
	if h.r.IsIDRemoved(uint64(from)) {
		plog.Warningf("rejected the stream from peer %s since it was removed", from)
		http.Error(w, "removed member", http.StatusGone)
		return
	}
	p := h.peerGetter.Get(from)
	if p == nil {
		// This may happen in following cases:
		// 1. user starts a remote peer that belongs to a different cluster
		// with the same cluster ID.
		// 2. local etcd falls behind of the cluster, and cannot recognize
		// the members that joined after its current progress.
		plog.Errorf("failed to find member %s in cluster %s", from, h.cid)
		http.Error(w, "error sender not found", http.StatusNotFound)
		return
	}

	wto := h.id.String()
	if gto := r.Header.Get("X-Raft-To"); gto != wto {
		plog.Errorf("streaming request ignored (ID mismatch got %s want %s)", gto, wto)
		http.Error(w, "to field mismatch", http.StatusPreconditionFailed)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()

	c := newCloseNotifier()
	conn := &outgoingConn{
		t:       t,
		termStr: r.Header.Get("X-Raft-Term"),
		Writer:  w,
		Flusher: w.(http.Flusher),
		Closer:  c,
	}
	p.attachOutgoingConn(conn)
	<-c.closeNotify()
}

// checkClusterCompatibilityFromHeader checks the cluster compatibility of
// the local member from the given header.
// It checks whether the version of local member is compatible with
// the versions in the header, and whether the cluster ID of local member
// matches the one in the header.
func checkClusterCompatibilityFromHeader(header http.Header, cid types.ID) error {
	if err := checkVersionCompability(header.Get("X-Server-From"), serverVersion(header), minClusterVersion(header)); err != nil {
		plog.Errorf("request version incompatibility (%v)", err)
		return errIncompatibleVersion
	}
	if gcid := header.Get("X-Etcd-Cluster-ID"); gcid != cid.String() {
		plog.Errorf("request cluster ID mismatch (got %s want %s)", gcid, cid)
		return errClusterIDMismatch
	}
	return nil
}

type closeNotifier struct {
	done chan struct{}
}

func newCloseNotifier() *closeNotifier {
	return &closeNotifier{
		done: make(chan struct{}),
	}
}

func (n *closeNotifier) Close() error {
	close(n.done)
	return nil
}

func (n *closeNotifier) closeNotify() <-chan struct{} { return n.done }
