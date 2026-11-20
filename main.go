// websockets-server is a self-hosted WebSocket server that speaks the Pusher protocol.
//
// Backends publish events over the Pusher HTTP API, signed with the application secret, and
// browsers connect with pusher-js or Laravel Echo and subscribe to public, private and presence
// channels. One process can serve several applications, each with its own credentials.
//
// Neither secrets nor event payloads are ever written to the log.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed web
var webFiles embed.FS

type server struct {
	apps     *registry
	upgrader websocket.Upgrader
	started  time.Time
	addr     string
}

func main() {
	addr := flag.String("addr", "127.0.0.1:6001", "address to listen on (use 0.0.0.0:port to accept outside connections)")
	configFile := flag.String("config", "", "JSON file with the list of apps, to serve several applications")
	envFile := flag.String("env", "", ".env file to read PUSHER_APP_ID, PUSHER_APP_KEY and PUSHER_APP_SECRET from")
	origins := flag.String("origins", "", "allowed browser origins, comma separated; supports subdomain wildcards such as https://*.example.com (empty allows all)")
	certFile := flag.String("cert", "", "TLS certificate (optional; without it the server expects a TLS-terminating proxy in front)")
	keyFile := flag.String("key", "", "private key of the TLS certificate")
	flag.Parse()

	apps, err := loadApps(*configFile, *envFile, splitOrigins(*origins))
	if err != nil {
		log.Fatal(err)
	}

	s := newServer(apps, *addr)

	httpServer := &http.Server{Addr: *addr, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}

	if *certFile != "" {
		log.Printf("Listening with TLS on %s (%s)", *addr, describe(apps))
		log.Fatal(httpServer.ListenAndServeTLS(*certFile, *keyFile))
	}

	log.Printf("Listening on %s (%s). Panel: http://%s", *addr, describe(apps), *addr)
	log.Fatal(httpServer.ListenAndServe())
}

func newServer(apps *registry, addr string) *server {
	return &server{
		apps:    apps,
		started: time.Now(),
		addr:    addr,
		// Origins are checked per application before upgrading, so the upgrader itself accepts any
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
	}
}

func (s *server) routes() http.Handler {
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()

	// Browsers (pusher-js, Laravel Echo)
	mux.HandleFunc("GET /app/{key}", s.handleSocket)

	// Backends (Pusher HTTP API)
	mux.HandleFunc("POST /apps/{app}/events", s.signed(s.handleEvents))
	mux.HandleFunc("POST /apps/{app}/batch_events", s.signed(s.handleBatchEvents))
	mux.HandleFunc("GET /apps/{app}/channels", s.signed(s.handleChannels))
	mux.HandleFunc("GET /apps/{app}/channels/{channel}", s.signed(s.handleChannel))

	// Status panel and its assets, reachable from this machine only
	files := http.FileServer(http.FS(static))

	mux.HandleFunc("GET /stats", localOnly(s.handleStats))
	mux.HandleFunc("POST /debug/send", localOnly(s.handleDebugSend))
	mux.Handle("GET /lib/", localOnly(files.ServeHTTP))
	mux.Handle("GET /", localOnly(files.ServeHTTP))

	return mux
}

func describe(apps *registry) string {
	names := make([]string, len(apps.list))
	for i, app := range apps.list {
		names[i] = app.Name
	}

	if len(names) == 1 {
		return "app " + names[0]
	}

	return "apps " + strings.Join(names, ", ")
}

// localOnly lets through only requests made from this machine and not relayed by a proxy.
func localOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		proxied := r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != ""

		if ip == nil || !ip.IsLoopback() || proxied {
			http.NotFound(w, r)
			return
		}

		next(w, r)
	}
}

// signed checks that the request comes from the application's backend: a known app id and a
// signature made with that app's secret.
func (s *server) signed(next func(http.ResponseWriter, *http.Request, *App, []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
			return
		}

		app := s.apps.byID[r.PathValue("app")]
		if app == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown application"})
			return
		}

		if err := verifyRequest(app, r.Method, r.URL.Path, r.URL.Query(), body, time.Now()); err != nil {
			log.Printf("[%s] request to %s rejected: %v", app.Name, r.URL.Path, err)
			app.hub.rejectedAPI.Add(1)
			app.hub.reject(r.URL.Path, err.Error(), "api")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}

		next(w, r, app, body)
	}
}

// apiEvent is an event published by a backend. `data` arrives already encoded as a JSON string.
type apiEvent struct {
	Name     string   `json:"name"`
	Data     string   `json:"data"`
	Channel  string   `json:"channel"`
	Channels []string `json:"channels"`
	SocketID string   `json:"socket_id"`
}

func (app *App) publish(e apiEvent, source string) {
	channels := e.Channels
	if e.Channel != "" {
		channels = append(channels, e.Channel)
	}

	for _, channel := range channels {
		app.hub.broadcast(channel, e.Name, e.Data, e.SocketID, source)
	}
}

func (s *server) handleEvents(w http.ResponseWriter, _ *http.Request, app *App, body []byte) {
	var e apiEvent

	if err := json.Unmarshal(body, &e); err != nil || e.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid event"})
		return
	}

	app.publish(e, "api")
	writeJSON(w, http.StatusOK, map[string]string{})
}

func (s *server) handleBatchEvents(w http.ResponseWriter, _ *http.Request, app *App, body []byte) {
	var batch struct {
		Batch []apiEvent `json:"batch"`
	}

	if err := json.Unmarshal(body, &batch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid batch"})
		return
	}

	for _, e := range batch.Batch {
		if e.Name != "" {
			app.publish(e, "api")
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{})
}

func (s *server) handleChannels(w http.ResponseWriter, r *http.Request, app *App, _ []byte) {
	prefix := r.URL.Query().Get("filter_by_prefix")
	channels := map[string]map[string]int{}

	for name, count := range app.hub.channelCounts() {
		if strings.HasPrefix(name, prefix) {
			channels[name] = map[string]int{"subscription_count": count}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"channels": channels})
}

func (s *server) handleChannel(w http.ResponseWriter, r *http.Request, app *App, _ []byte) {
	count := app.hub.channelCounts()[r.PathValue("channel")]

	writeJSON(w, http.StatusOK, map[string]any{"occupied": count > 0, "subscription_count": count})
}

// panelApp returns the application the panel is looking at: the one named in ?app=, or the first.
func (s *server) panelApp(r *http.Request) *App {
	if app := s.apps.byID[r.URL.Query().Get("app")]; app != nil {
		return app
	}

	return s.apps.list[0]
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)

	app := s.panelApp(r)

	stats := app.hub.snapshot()
	stats.App = appInfo{ID: app.ID, Name: app.Name, Connections: stats.Connections}
	stats.AppKey = app.Key
	stats.Addr = s.addr
	stats.UptimeSeconds = int64(time.Since(s.started).Seconds())
	stats.Runtime = runtimeInfo{
		GoVersion:  strings.SplitN(runtime.Version(), "-", 2)[0],
		Goroutines: runtime.NumGoroutine(),
		MemoryMB:   float64(memory.HeapInuse+memory.StackInuse) / (1 << 20),
	}

	for _, other := range s.apps.list {
		stats.Apps = append(stats.Apps, appInfo{ID: other.ID, Name: other.Name, Connections: other.hub.connectionCount()})
	}

	writeJSON(w, http.StatusOK, stats)
}

// handleDebugSend publishes a test event from the panel, without going through a backend.
func (s *server) handleDebugSend(w http.ResponseWriter, r *http.Request) {
	var e apiEvent

	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&e); err != nil || e.Name == "" || e.Channel == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing channel or event name"})
		return
	}

	s.panelApp(r).publish(e, "panel")
	writeJSON(w, http.StatusOK, map[string]string{})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
