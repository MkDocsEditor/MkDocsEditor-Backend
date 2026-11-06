package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitstore"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/metrics"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v5"
	"github.com/prometheus/client_golang/prometheus"
)

// unmatchedRoute is the route label of requests that match no route. The request path is never
// a label: anyone could create new time series with it.
const unmatchedRoute = "unmatched"

// isAliveCheck reports whether the request is a check of GET /alive/, which a readiness probe
// sends every few seconds. Logs and request metrics leave these out: they would drown the requests
// of real clients.
func isAliveCheck(c *echo.Context) bool {
	return c.Request().Method == http.MethodGet && c.Path() == EndpointPathAlive
}

// httpMetrics counts the requests by route pattern, except for checks of GET /alive/. It must run
// outside the middleware that turns errors into responses, so it sees the final status.
func (rs *RestService) httpMetrics(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if isAliveCheck(c) {
			return next(c)
		}
		start := time.Now()
		err := next(c)

		req := c.Request()
		route := c.Path()
		if _, ok := rs.knownRoutes[route]; !ok {
			route = unmatchedRoute
		}
		_, status := echo.ResolveResponseStatus(c.Response(), err)
		isWebsocket := websocket.IsWebSocketUpgrade(req)
		if isWebsocket && status == http.StatusOK {
			// a successful upgrade hijacks the connection and leaves the status untouched
			status = http.StatusSwitchingProtocols
		}
		method := metricsMethod(req.Method)
		metrics.HTTPRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
		if !isWebsocket {
			metrics.HTTPRequestDuration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
		}
		return err
	}
}

// metricsMethod returns the method label. Clients can send any method name, so the ones the API
// does not use are collected in one value.
func metricsMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return method
	}
	return "other"
}

// knownRoutesOf returns the route patterns of a server
func knownRoutesOf(e *echo.Echo) map[string]struct{} {
	known := map[string]struct{}{}
	for _, r := range e.Router().Routes() {
		known[r.Path] = struct{}{}
	}
	return known
}

// countTree returns the number of sections, documents and resources below a section, itself
// included
func countTree(section *wiki.Section) (sections int, documents int, resources int) {
	if section == nil {
		return 0, 0, 0
	}
	sections, documents, resources = 1, len(section.Documents), len(section.Resources)
	for _, sub := range section.Subsections {
		s, d, r := countTree(sub)
		sections += s
		documents += d
		resources += r
	}
	return sections, documents, resources
}

// MetricsCollector reports the current state of the backend at scrape time, so these values
// cannot drift away from it
type MetricsCollector struct {
	sync *collab.SyncServer
	tree *wiki.TreeManager
	// git is nil if git is disabled
	git *gitstore.Committer

	loadedDocuments *prometheus.Desc
	dirtyDocuments  *prometheus.Desc
	connections     *prometheus.Desc
	onlineUsers     *prometheus.Desc
	eventListeners  *prometheus.Desc
	treeItems       *prometheus.Desc
	gitPendingDocs  *prometheus.Desc
	gitPendingEdits *prometheus.Desc
}

// NewMetricsCollector creates the collector. git may be nil.
func NewMetricsCollector(syncServer *collab.SyncServer, tree *wiki.TreeManager, git *gitstore.Committer) *MetricsCollector {
	desc := func(name string, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(metrics.Namespace+"_"+name, help, labels, nil)
	}
	return &MetricsCollector{
		sync:            syncServer,
		tree:            tree,
		git:             git,
		loadedDocuments: desc("documents_loaded", "Documents loaded in memory."),
		dirtyDocuments:  desc("documents_dirty", "Loaded documents with changes that are not on disk yet."),
		connections:     desc("document_connections", "Open document websockets, by access (write, read).", "access"),
		onlineUsers:     desc("users_online", "Distinct users with a document open."),
		eventListeners:  desc("event_connections", "Open events websockets."),
		treeItems:       desc("tree_items", "Items in the wiki, by type (section, document, resource).", "type"),
		gitPendingDocs:  desc("git_pending_documents", "Documents with changes that are not committed yet."),
		gitPendingEdits: desc("git_pending_changes", "Automerge changes that are not committed yet."),
	}
}

// Describe implements prometheus.Collector
func (m *MetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		m.loadedDocuments, m.dirtyDocuments, m.connections, m.onlineUsers, m.eventListeners,
		m.treeItems, m.gitPendingDocs, m.gitPendingEdits,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector
func (m *MetricsCollector) Collect(ch chan<- prometheus.Metric) {
	gauge := func(d *prometheus.Desc, value int, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, float64(value), labels...)
	}
	stats := m.sync.Stats()
	gauge(m.loadedDocuments, stats.LoadedDocuments)
	gauge(m.dirtyDocuments, stats.DirtyDocuments)
	gauge(m.connections, stats.Writers, "write")
	gauge(m.connections, stats.Readers, "read")
	gauge(m.onlineUsers, stats.OnlineUsers)
	gauge(m.eventListeners, stats.EventListeners)

	sections, documents, resources := countTree(m.tree.Root())
	gauge(m.treeItems, sections, "section")
	gauge(m.treeItems, documents, "document")
	gauge(m.treeItems, resources, "resource")

	if m.git != nil {
		pendingDocuments, pendingChanges := m.git.PendingStats()
		gauge(m.gitPendingDocs, pendingDocuments)
		gauge(m.gitPendingEdits, pendingChanges)
	}
}
