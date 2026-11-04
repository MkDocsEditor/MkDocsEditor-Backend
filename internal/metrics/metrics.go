// Package metrics holds the Prometheus metrics of the backend and the server that exposes them.
// The metrics are always recorded. They are only exposed when server.metrics.enabled is set.
//
// Labels stay low-cardinality: no document paths and nothing a client chooses. Per-document
// activity is in the sync event log, see docs/metrics.md.
package metrics

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Namespace is the prefix of all metrics of the backend
const Namespace = "mkdocseditor"

// Values of the label "result"
const (
	ResultOK    = "ok"
	ResultError = "error"
	// ResultEmpty is a git commit that had nothing to commit
	ResultEmpty = "empty"
)

// Values of the label "direction"
const (
	DirectionReceived = "received"
	DirectionSent     = "sent"
)

// Values of the label "type" of WebsocketConnections
const (
	ConnectionDocument = "document"
	ConnectionEvents   = "events"
)

// Values of the label "reason" of SyncRejected
const (
	RejectInvalidMessage = "invalid-message"
	RejectApplyFailed    = "apply-failed"
	RejectForeignActor   = "foreign-actor"
	RejectReadOnly       = "read-only"
)

var (
	// Registry holds only the metrics of this backend and of its Go process
	Registry = prometheus.NewRegistry()

	factory = promauto.With(Registry)

	// latencyBuckets cover the operations that run while users wait, in seconds
	latencyBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}
	// sizeBuckets cover 1 KiB to 64 MiB, the size limit of a websocket message
	sizeBuckets = prometheus.ExponentialBuckets(1024, 4, 9)
)

// REST API
var (
	HTTPRequests = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "http_requests_total",
		Help:      "Finished HTTP requests. A websocket counts when it closes, with status 101 if the upgrade succeeded.",
	}, []string{"method", "route", "status"})

	HTTPRequestDuration = factory.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: Namespace,
		Name:      "http_request_duration_seconds",
		Help:      "Duration of HTTP requests, without websockets.",
		Buckets:   latencyBuckets,
	}, []string{"method", "route"})
)

// Websockets
var (
	WebsocketConnections = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "websocket_connections_total",
		Help:      "Accepted websocket upgrades by type (document, events).",
	}, []string{"type"})

	WebsocketSendQueueFull = factory.NewCounter(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "websocket_send_queue_full_total",
		Help:      "Clients disconnected because they did not keep up with their messages.",
	})

	WebsocketHeartbeatTimeouts = factory.NewCounter(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "websocket_heartbeat_timeouts_total",
		Help:      "Clients disconnected because they stopped sending heartbeats.",
	})

	PresenceMessages = factory.NewCounter(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "presence_messages_total",
		Help:      "Presence (cursor and selection) messages received from clients.",
	})
)

// Automerge sync
var (
	SyncMessages = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "sync_messages_total",
		Help:      "Automerge sync messages by direction (received, sent).",
	}, []string{"direction"})

	SyncBytes = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "sync_bytes_total",
		Help:      "Bytes of Automerge sync messages by direction (received, sent), before base64.",
	}, []string{"direction"})

	SyncChanges = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "sync_changes_total",
		Help:      "Automerge changes in sync messages that changed a document, by user.",
	}, []string{"user"})

	SyncApplyDuration = factory.NewHistogram(prometheus.HistogramOpts{
		Namespace: Namespace,
		Name:      "sync_apply_duration_seconds",
		Help:      "Time to apply a sync message of a client to the server document.",
		Buckets:   latencyBuckets,
	})

	SyncRejected = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "sync_rejected_total",
		Help:      "Sync messages that were not applied, by reason (invalid-message, apply-failed, foreign-actor, read-only).",
	}, []string{"reason"})

	ExternalChanges = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "external_changes_total",
		Help:      "File changes on disk merged into a document, by result.",
	}, []string{"result"})
)

// Documents
var (
	DocumentLoads = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "document_loads_total",
		Help:      "Documents loaded into memory, by result.",
	}, []string{"result"})

	DocumentLoadDuration = factory.NewHistogram(prometheus.HistogramOpts{
		Namespace: Namespace,
		Name:      "document_load_duration_seconds",
		Help:      "Time to load a document and merge its file content.",
		Buckets:   latencyBuckets,
	})

	DocumentStateCorrupt = factory.NewCounter(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "document_state_corrupt_total",
		Help:      "Saved Automerge states that could not be loaded, so the document started a new epoch.",
	})

	DocumentFlushes = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "document_flushes_total",
		Help:      "Writes of a document to its file and the state directory, by result. Failed flushes are retried.",
	}, []string{"result"})

	DocumentFlushDuration = factory.NewHistogram(prometheus.HistogramOpts{
		Namespace: Namespace,
		Name:      "document_flush_duration_seconds",
		Help:      "Time to write a document to its file and the state directory.",
		Buckets:   latencyBuckets,
	})

	DocumentStateSize = factory.NewHistogram(prometheus.HistogramOpts{
		Namespace: Namespace,
		Name:      "document_state_size_bytes",
		Help:      "Size of the saved Automerge state at each flush.",
		Buckets:   sizeBuckets,
	})

	TreeRebuilds = factory.NewCounter(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "tree_rebuilds_total",
		Help:      "Rebuilds of the item tree (file watcher and REST changes).",
	})
)

// Git
var (
	GitCommits = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "git_commits_total",
		Help:      "Git commit attempts, by result (ok, empty, error).",
	}, []string{"result"})

	GitCommitDuration = factory.NewHistogram(prometheus.HistogramOpts{
		Namespace: Namespace,
		Name:      "git_commit_duration_seconds",
		Help:      "Time to create a git commit.",
		Buckets:   latencyBuckets,
	})

	GitPushes = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "git_pushes_total",
		Help:      "Git pushes, by result.",
	}, []string{"result"})
)

// Accounts
var (
	AuthEvents = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "auth_events_total",
		Help:      "Security events of the audit log, by type (see docs/auth.md).",
	}, []string{"type"})
)

var buildInfo = factory.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: Namespace,
	Name:      "build_info",
	Help:      "Always 1. The labels tell the version of the running backend.",
}, []string{"version", "commit"})

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// SetBuildInfo publishes the version of the running backend
func SetBuildInfo(version string, commit string) {
	buildInfo.WithLabelValues(version, commit).Set(1)
}

// Server exposes the metrics at /metrics on a listener of its own, so they never become part
// of the API that is published through the reverse proxy
type Server struct {
	http     *http.Server
	listener net.Listener
}

// Listen opens the listener of the metrics server. Serving starts with Serve.
func Listen(addr string) (*Server, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Registry: Registry}))
	return &Server{
		listener: listener,
		http: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout:      30 * time.Second,
		},
	}, nil
}

// Addr returns the address the server listens on
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Serve answers scrapes until Shutdown. A failure only ends the metrics, not the backend.
func (s *Server) Serve() {
	if err := s.http.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("The metrics server stopped: %v", err)
	}
}

// Shutdown stops the server
func (s *Server) Shutdown(ctx context.Context) {
	if err := s.http.Shutdown(ctx); err != nil {
		log.Printf("Unable to stop the metrics server: %v", err)
	}
}
