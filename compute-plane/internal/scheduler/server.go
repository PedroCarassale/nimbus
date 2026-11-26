package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/registry"
	ntrace "github.com/PedroCarassale/nimbus/compute-plane/internal/trace"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Server is the HTTP front of the scheduler. The paths match the compute
// plane so Nest only changes COMPUTE_PLANE_URL.
type Server struct {
	reg      *registry.Registry
	picker   *Picker
	defaults fnconfig.Defaults
	client   *http.Client
}

func New(reg *registry.Registry, defaults fnconfig.Defaults, client *http.Client) *Server {
	if client == nil {
		client = newClient(2*time.Second, RequestBudget())
	}
	if defaults.TimeoutSec == 0 || defaults.MemoryMB == 0 || defaults.Handler == "" {
		defaults = fnconfig.LoadDefaults()
	}
	return &Server{
		reg:      reg,
		picker:   NewPicker(),
		defaults: defaults,
		client:   client,
	}
}

// RequestBudget covers the longest accepted execution plus the queue wait.
func RequestBudget() time.Duration {
	wait := 30
	if raw := strings.TrimSpace(os.Getenv("NIMBUS_FN_QUEUE_WAIT_SEC")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			wait = n
		}
	}
	return time.Duration(fnconfig.TimeoutMax+wait)*time.Second + 45*time.Second
}

func newClient(dial, budget time.Duration) *http.Client {
	if dial <= 0 {
		dial = 2 * time.Second
	}
	return &http.Client{
		Timeout: budget,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   dial,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   2 * time.Second,
			ResponseHeaderTimeout: budget,
			ExpectContinueTimeout: time.Second,
		},
	}
}

// Handler is the scheduler HTTP mux.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(RequestBudget()))
	r.Get("/health", s.health)
	r.Head("/health", s.health)
	r.Handle("/metrics", promhttp.Handler())
	r.Get("/workers", s.workers)
	r.Post("/executions", s.executions)
	r.Post("/executions/{id}/cancel", s.cancel)
	r.Post("/async/jobs", s.forwardAny)
	r.Get("/dlq", s.forwardAny)
	r.Post("/dlq/{id}/redrive", s.forwardAny)
	return r
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	n := s.healthyCount(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"service":        "scheduler",
		"workersHealthy": n,
	})
}

func (s *Server) workers(w http.ResponseWriter, r *http.Request) {
	snaps := s.snapshots(r.Context())
	if snaps == nil {
		snaps = []registry.Snapshot{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": snaps})
}

func (s *Server) executions(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx := ntrace.ExtractHTTP(r.Context(), r.Header)
	ctx, span := ntrace.Start(ctx, "scheduler.route", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()

	workers := s.routeWorkers(ctx)
	key := affinityKey(body, s.defaults)
	chosen, reason, ok := s.picker.Choose(workers, key, nil)
	if !ok {
		span.SetStatus(codes.Error, "no workers")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "No hay workers de compute disponibles",
		})
		return
	}

	resp, ferr := s.forward(ctx, chosen.URL, r, body)
	if IsDialError(ferr) {
		s.picker.Done(chosen.ID)
		log.Printf("[scheduler] dial %s falló, reintento: %v", chosen.ID, ferr)
		alt, _, okAlt := s.picker.Choose(workers, key, map[string]bool{chosen.ID: true})
		if !okAlt {
			span.SetAttributes(
				attribute.String("nimbus.worker_id", chosen.ID),
				attribute.String("nimbus.route_reason", "failover"),
			)
			span.SetStatus(codes.Error, "failover failed")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "No hay workers de compute disponibles",
			})
			return
		}
		chosen = alt
		reason = "failover"
		resp, ferr = s.forward(ctx, chosen.URL, r, body)
	}
	s.picker.Done(chosen.ID)
	span.SetAttributes(
		attribute.String("nimbus.worker_id", chosen.ID),
		attribute.String("nimbus.route_reason", reason),
	)
	if ferr != nil {
		ntrace.Fail(span, ferr)
		log.Printf("[scheduler] route worker=%s reason=%s: %v", chosen.ID, reason, ferr)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "No se pudo contactar al worker de compute",
		})
		return
	}
	recordRoute(chosen.ID, reason)
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	writeUpstream(w, resp)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx := ntrace.ExtractHTTP(r.Context(), r.Header)
	ctx, span := ntrace.Start(ctx, "scheduler.route", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()

	id := chi.URLParam(r, "id")
	snaps := s.snapshots(ctx)
	workerID, lookErr := s.reg.ExecutionWorker(ctx, id)
	if lookErr != nil {
		log.Printf("[scheduler] worker de %s: %v", id, lookErr)
	}
	if workerID != "" {
		if snap, ok := findSnap(snaps, workerID); ok {
			resp, ferr := s.forward(ctx, snap.URL, r, body)
			if ferr == nil {
				recordRoute(workerID, "cancel")
				span.SetAttributes(
					attribute.String("nimbus.worker_id", workerID),
					attribute.String("nimbus.route_reason", "cancel"),
					attribute.Int("http.response.status_code", resp.StatusCode),
				)
				writeUpstream(w, resp)
				return
			}
			log.Printf("[scheduler] cancel directo %s: %v", workerID, ferr)
		}
	}

	if len(snaps) == 0 {
		span.SetStatus(codes.Error, "no workers")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "No hay workers de compute disponibles",
		})
		return
	}
	var hits []cancelHit
	for _, snap := range snaps {
		if snap.URL == "" || (workerID != "" && snap.WorkerID == workerID) {
			continue
		}
		resp, ferr := s.forward(ctx, snap.URL, r, body)
		if ferr != nil {
			log.Printf("[scheduler] cancel broadcast %s: %v", snap.WorkerID, ferr)
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		hits = append(hits, cancelHit{worker: snap.WorkerID, status: resp.StatusCode, header: resp.Header, body: raw})
	}
	chosen := pickCancel(hits)
	if chosen == nil {
		span.SetStatus(codes.Error, "broadcast failed")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "No se pudo contactar al worker de compute",
		})
		return
	}
	recordRoute(chosen.worker, "broadcast")
	span.SetAttributes(
		attribute.String("nimbus.worker_id", chosen.worker),
		attribute.String("nimbus.route_reason", "broadcast"),
		attribute.Int("http.response.status_code", chosen.status),
	)
	copyHeader(w.Header(), chosen.header)
	w.WriteHeader(chosen.status)
	_, _ = w.Write(chosen.body)
}

func (s *Server) forwardAny(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx := ntrace.ExtractHTTP(r.Context(), r.Header)
	ctx, span := ntrace.Start(ctx, "scheduler.route", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()

	workers := s.routeWorkers(ctx)
	chosen, _, ok := s.picker.Choose(workers, "", nil)
	if !ok {
		span.SetStatus(codes.Error, "no workers")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "No hay workers de compute disponibles",
		})
		return
	}
	reason := "any"
	resp, ferr := s.forward(ctx, chosen.URL, r, body)
	if IsDialError(ferr) {
		s.picker.Done(chosen.ID)
		alt, _, okAlt := s.picker.Choose(workers, "", map[string]bool{chosen.ID: true})
		if !okAlt {
			span.SetStatus(codes.Error, "failover failed")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "No hay workers de compute disponibles",
			})
			return
		}
		chosen = alt
		reason = "failover"
		resp, ferr = s.forward(ctx, chosen.URL, r, body)
	}
	s.picker.Done(chosen.ID)
	span.SetAttributes(
		attribute.String("nimbus.worker_id", chosen.ID),
		attribute.String("nimbus.route_reason", reason),
	)
	if ferr != nil {
		ntrace.Fail(span, ferr)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "No se pudo contactar al worker de compute",
		})
		return
	}
	recordRoute(chosen.ID, reason)
	writeUpstream(w, resp)
}

func (s *Server) snapshots(ctx context.Context) []registry.Snapshot {
	if s.reg == nil {
		setHealthy(0)
		return nil
	}
	snaps, err := s.reg.List(ctx)
	if err != nil {
		log.Printf("[scheduler] listando workers: %v", err)
		setHealthy(0)
		return nil
	}
	if snaps == nil {
		snaps = []registry.Snapshot{}
	}
	n := 0
	for _, snap := range snaps {
		if snap.State == registry.StateHealthy {
			n++
		}
	}
	setHealthy(n)
	return snaps
}

func (s *Server) healthyCount(ctx context.Context) int {
	snaps := s.snapshots(ctx)
	n := 0
	for _, snap := range snaps {
		if snap.State == registry.StateHealthy {
			n++
		}
	}
	return n
}

func (s *Server) routeWorkers(ctx context.Context) []Worker {
	snaps := s.snapshots(ctx)
	out := make([]Worker, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, Worker{
			ID:           snap.WorkerID,
			URL:          snap.URL,
			Active:       snap.ActiveExecutions,
			Max:          snap.MaxContainers,
			IdleWarmKeys: snap.IdleWarmKeys,
			State:        snap.State,
			Live:         true,
		})
	}
	return out
}

func (s *Server) forward(ctx context.Context, base string, in *http.Request, body []byte) (*http.Response, error) {
	target := strings.TrimRight(base, "/") + in.URL.RequestURI()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, in.Method, target, rdr)
	if err != nil {
		return nil, err
	}
	copyHeader(req.Header, in.Header)
	ntrace.InjectHTTP(ctx, req.Header)
	return s.client.Do(req)
}

func findSnap(snaps []registry.Snapshot, id string) (registry.Snapshot, bool) {
	for _, snap := range snaps {
		if snap.WorkerID == id && snap.URL != "" {
			return snap, true
		}
	}
	return registry.Snapshot{}, false
}

type cancelHit struct {
	worker string
	status int
	header http.Header
	body   []byte
}

func pickCancel(hits []cancelHit) *cancelHit {
	var fallback *cancelHit
	for i := range hits {
		h := &hits[i]
		if cancelledTrue(h.body) {
			return h
		}
		if h.status >= 200 && h.status < 300 && fallback == nil {
			fallback = h
		}
	}
	if fallback != nil {
		return fallback
	}
	if len(hits) > 0 {
		return &hits[len(hits)-1]
	}
	return nil
}

func cancelledTrue(body []byte) bool {
	var payload struct {
		Cancelled bool `json:"cancelled"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	return payload.Cancelled
}

// IsDialError reports a failure to open the connection. A response that
// already started is not a dial error: the execution may have begun, and
// the scheduler does not send it to a second worker.
func IsDialError(err error) bool {
	if err == nil {
		return false
	}
	var op *net.OpError
	if errors.As(err, &op) && (op.Op == "dial" || op.Op == "connect") {
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() && errors.Is(urlErr.Err, context.DeadlineExceeded) {
		return false
	}
	return false
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errors.New("El cuerpo de la solicitud supera 1 MB")
		}
		return nil, errors.New("No se pudo leer el cuerpo de la solicitud")
	}
	return body, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeUpstream(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func copyHeader(dst, src http.Header) {
	for k, vals := range src {
		if hopHeader(k) {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func hopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade", "content-length":
		return true
	default:
		return false
	}
}
