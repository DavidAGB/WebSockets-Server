package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// How often the client must show signs of life; pusher-js sends a ping after this long without activity
	activityTimeout = 30 * time.Second
	// With no sign of life for this long the connection is considered dead
	readTimeout  = activityTimeout*2 + 15*time.Second
	writeTimeout = 10 * time.Second

	maxClientMessage = 64 << 10
	sendBuffer       = 128
	recentEvents     = 200

	// The panel charts the last five minutes, one sample per second
	historySeconds = 300
	// How many connections and channel members are listed for the panel
	listedSockets = 200
	listedMembers = 100
)

// Error codes of the Pusher protocol.
const (
	codeUnknownApp   = 4001
	codeUnauthorized = 4009
	codeNotSupported = 4301
	codeClientEvents = 4302
)

// message is the envelope of the Pusher protocol in both directions.
type message struct {
	Event   string          `json:"event"`
	Channel string          `json:"channel,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Conn is one connected browser.
type Conn struct {
	id   string
	hub  *Hub
	ws   *websocket.Conn
	send chan []byte
	once sync.Once

	// Connection details shown in the panel
	connectedAt time.Time
	remote      string
	origin      string
	agent       string
	sent        atomic.Int64

	// Guarded by Hub.mu
	channels map[string]bool
	members  map[string]string // presence channel → user_id it joined with
}

func (c *Conn) close() {
	c.once.Do(func() { close(c.send) })
}

// member is a user inside a presence channel; the same user may have several tabs open.
type member struct {
	info  json.RawMessage
	conns int
}

// Hub tracks which connection is subscribed to which channel and fans events out.
type Hub struct {
	mu       sync.RWMutex
	conns    map[string]*Conn
	channels map[string]map[*Conn]bool
	presence map[string]map[string]*member

	published  int64
	delivered  int64
	totalConns int64
	peakConns  int
	nextLogID  int64
	recent     []eventLog
	history    []sample

	// Counted outside the lock: they happen while others hold it
	droppedSlow  atomic.Int64
	rejectedSubs atomic.Int64
	rejectedAPI  atomic.Int64
}

// eventLog is one line of the panel's activity feed: an event that was fanned out or an attempt
// that was rejected. It keeps the size of the event, never its content.
type eventLog struct {
	ID        int64     `json:"id"`
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"` // event | reject
	Channel   string    `json:"channel"`
	Event     string    `json:"event"`
	Receivers int       `json:"receivers"`
	Bytes     int       `json:"bytes"`
	Source    string    `json:"source"`
}

// sample is what happened during one second.
type sample struct {
	connections int
	published   int64
	delivered   int64
}

type statsSnapshot struct {
	App           appInfo   `json:"app"`
	Apps          []appInfo `json:"apps"`
	AppKey        string    `json:"appKey"`
	Addr          string    `json:"addr"`
	UptimeSeconds int64     `json:"uptimeSeconds"`

	Connections      int   `json:"connections"`
	PeakConnections  int   `json:"peakConnections"`
	TotalConnections int64 `json:"totalConnections"`
	Published        int64 `json:"published"`
	Delivered        int64 `json:"delivered"`

	RejectedSubscriptions int64 `json:"rejectedSubscriptions"`
	RejectedRequests      int64 `json:"rejectedRequests"`
	DroppedSlow           int64 `json:"droppedSlow"`

	Channels []channelInfo `json:"channels"`
	Sockets  []socketInfo  `json:"sockets"`
	Recent   []eventLog    `json:"recent"`
	History  historyInfo   `json:"history"`
	Runtime  runtimeInfo   `json:"runtime"`
}

// appInfo identifies an application in the panel's selector.
type appInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Connections int    `json:"connections"`
}

type channelInfo struct {
	Name        string       `json:"name"`
	Type        string       `json:"type"` // public | private | presence
	Subscribers int          `json:"subscribers"`
	Users       int          `json:"users"`
	Members     []memberInfo `json:"members,omitempty"`
}

type memberInfo struct {
	ID   string          `json:"id"`
	Info json.RawMessage `json:"info"`
}

type socketInfo struct {
	ID          string    `json:"id"`
	ConnectedAt time.Time `json:"connectedAt"`
	Remote      string    `json:"remote"`
	Origin      string    `json:"origin"`
	Agent       string    `json:"agent"`
	Channels    []string  `json:"channels"`
	Sent        int64     `json:"sent"`
}

// historyInfo holds the per-second samples, oldest first; the last one belongs to `end`.
type historyInfo struct {
	End         time.Time `json:"end"`
	Connections []int     `json:"connections"`
	Published   []int64   `json:"published"`
	Delivered   []int64   `json:"delivered"`
}

type runtimeInfo struct {
	GoVersion  string  `json:"goVersion"`
	Goroutines int     `json:"goroutines"`
	MemoryMB   float64 `json:"memoryMB"`
}

func newHub() *Hub {
	return &Hub{
		conns:    map[string]*Conn{},
		channels: map[string]map[*Conn]bool{},
		presence: map[string]map[string]*member{},
	}
}

// runSampler records, every second, how many connections there are and how much was published
// and delivered during that second.
func (h *Hub) runSampler() {
	var lastPublished, lastDelivered int64

	for range time.Tick(time.Second) {
		h.mu.Lock()
		h.history = append(h.history, sample{
			connections: len(h.conns),
			published:   h.published - lastPublished,
			delivered:   h.delivered - lastDelivered,
		})
		if len(h.history) > historySeconds {
			h.history = h.history[len(h.history)-historySeconds:]
		}
		lastPublished, lastDelivered = h.published, h.delivered
		h.mu.Unlock()
	}
}

func (h *Hub) add(ws *websocket.Conn, r *http.Request) *Conn {
	h.mu.Lock()
	defer h.mu.Unlock()

	c := &Conn{
		hub: h, ws: ws, send: make(chan []byte, sendBuffer), channels: map[string]bool{}, members: map[string]string{},
		connectedAt: time.Now(), remote: clientIP(r), origin: r.Header.Get("Origin"), agent: r.UserAgent(),
	}

	// The protocol requires socket ids shaped like "number.number"
	for c.id == "" || h.conns[c.id] != nil {
		c.id = fmt.Sprintf("%d.%d", rand.Uint32N(1_000_000_000)+1, rand.Uint32N(1_000_000_000)+1)
	}

	h.conns[c.id] = c
	h.totalConns++
	h.peakConns = max(h.peakConns, len(h.conns))

	return c
}

func (h *Hub) remove(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for channel := range c.channels {
		h.leave(c, channel)
	}

	delete(h.conns, c.id)
	c.close()
}

// subscribe adds the connection to a public or private channel and confirms it.
func (h *Hub) subscribe(c *Conn, channel string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.enter(c, channel)
	c.sendFrame(encodeEvent("pusher_internal:subscription_succeeded", channel, map[string]any{}))
}

// joinPresence adds the connection to a presence channel: it replies with who is there and,
// unless the user already had another tab inside, tells the others that the user joined.
func (h *Hub) joinPresence(c *Conn, channel, userID string, info json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if c.channels[channel] {
		return
	}

	h.enter(c, channel)

	if h.presence[channel] == nil {
		h.presence[channel] = map[string]*member{}
	}

	m := h.presence[channel][userID]
	isNew := m == nil

	if isNew {
		m = &member{info: info}
		h.presence[channel][userID] = m
	}

	m.conns++
	c.members[channel] = userID

	ids := make([]string, 0, len(h.presence[channel]))
	hash := make(map[string]json.RawMessage, len(h.presence[channel]))

	for id, other := range h.presence[channel] {
		ids = append(ids, id)
		hash[id] = other.info
	}

	sort.Strings(ids)

	c.sendFrame(encodeEvent("pusher_internal:subscription_succeeded", channel, map[string]any{
		"presence": map[string]any{"ids": ids, "hash": hash, "count": len(ids)},
	}))

	if isNew {
		h.fanout(channel, encodeEvent("pusher_internal:member_added", channel, map[string]any{"user_id": userID, "user_info": info}), c.id)
	}
}

func (h *Hub) unsubscribe(c *Conn, channel string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.leave(c, channel)
}

// enter must be called with h.mu held.
func (h *Hub) enter(c *Conn, channel string) {
	if h.channels[channel] == nil {
		h.channels[channel] = map[*Conn]bool{}
	}

	h.channels[channel][c] = true
	c.channels[channel] = true
}

// leave must be called with h.mu held. On a presence channel it announces the departure when
// the user's last tab leaves.
func (h *Hub) leave(c *Conn, channel string) {
	delete(h.channels[channel], c)
	delete(c.channels, channel)

	if len(h.channels[channel]) == 0 {
		delete(h.channels, channel)
	}

	userID, isMember := c.members[channel]
	if !isMember {
		return
	}

	delete(c.members, channel)

	if m := h.presence[channel][userID]; m != nil {
		m.conns--

		if m.conns <= 0 {
			delete(h.presence[channel], userID)
			h.fanout(channel, encodeEvent("pusher_internal:member_removed", channel, map[string]any{"user_id": userID}), "")
		}
	}

	if len(h.presence[channel]) == 0 {
		delete(h.presence, channel)
	}
}

func (h *Hub) isSubscribed(c *Conn, channel string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return c.channels[channel]
}

// broadcast delivers the event to the channel's subscribers, except `exceptSocket` (its sender).
// `data` is the payload already serialized as a JSON string, the way backends publish it.
func (h *Hub) broadcast(channel, event, data, exceptSocket, source string) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}

	h.deliver(channel, message{Event: event, Channel: channel, Data: payload}, exceptSocket, source)
}

func (h *Hub) deliver(channel string, m message, exceptSocket, source string) {
	frame, err := json.Marshal(m)
	if err != nil {
		return
	}

	h.mu.RLock()
	receivers := h.fanout(channel, frame, exceptSocket)
	h.mu.RUnlock()

	h.mu.Lock()
	h.published++
	h.delivered += int64(receivers)
	h.log(eventLog{Kind: "event", Channel: channel, Event: m.Event, Receivers: receivers, Bytes: len(m.Data), Source: source})
	h.mu.Unlock()
}

// reject records a subscription or request that was refused.
func (h *Hub) reject(target, reason, source string) {
	h.mu.Lock()
	h.log(eventLog{Kind: "reject", Channel: target, Event: reason, Source: source})
	h.mu.Unlock()
}

// log must be called with h.mu held.
func (h *Hub) log(entry eventLog) {
	h.nextLogID++
	entry.ID = h.nextLogID
	entry.Time = time.Now()

	h.recent = append([]eventLog{entry}, h.recent...)
	if len(h.recent) > recentEvents {
		h.recent = h.recent[:recentEvents]
	}
}

// fanout queues the frame for every subscriber of the channel and returns how many got it.
// Must be called with h.mu held.
func (h *Hub) fanout(channel string, frame []byte, exceptSocket string) int {
	receivers := 0

	for c := range h.channels[channel] {
		if c.id != exceptSocket && c.sendFrame(frame) {
			receivers++
		}
	}

	return receivers
}

func (h *Hub) connectionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return len(h.conns)
}

func (h *Hub) channelCounts() map[string]int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	counts := make(map[string]int, len(h.channels))
	for name, members := range h.channels {
		counts[name] = len(members)
	}

	return counts
}

func (h *Hub) snapshot() statsSnapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()

	stats := statsSnapshot{
		Connections:           len(h.conns),
		PeakConnections:       h.peakConns,
		TotalConnections:      h.totalConns,
		Published:             h.published,
		Delivered:             h.delivered,
		RejectedSubscriptions: h.rejectedSubs.Load(),
		RejectedRequests:      h.rejectedAPI.Load(),
		DroppedSlow:           h.droppedSlow.Load(),
		Channels:              make([]channelInfo, 0, len(h.channels)),
		Sockets:               make([]socketInfo, 0, min(len(h.conns), listedSockets)),
		Recent:                append([]eventLog{}, h.recent...),
	}

	for name, subscribers := range h.channels {
		info := channelInfo{Name: name, Type: channelType(name), Subscribers: len(subscribers), Users: len(h.presence[name])}

		for id, m := range h.presence[name] {
			if len(info.Members) == listedMembers {
				break
			}

			info.Members = append(info.Members, memberInfo{ID: id, Info: m.info})
		}

		sort.Slice(info.Members, func(i, j int) bool { return info.Members[i].ID < info.Members[j].ID })
		stats.Channels = append(stats.Channels, info)
	}

	sort.Slice(stats.Channels, func(i, j int) bool { return stats.Channels[i].Name < stats.Channels[j].Name })

	// The panel gets the most recent connections; with thousands open, listing them all is pointless
	newest := make([]*Conn, 0, len(h.conns))
	for _, c := range h.conns {
		newest = append(newest, c)
	}

	sort.Slice(newest, func(i, j int) bool { return newest[i].connectedAt.After(newest[j].connectedAt) })

	for _, c := range newest[:min(len(newest), listedSockets)] {
		channels := make([]string, 0, len(c.channels))
		for name := range c.channels {
			channels = append(channels, name)
		}

		sort.Strings(channels)

		stats.Sockets = append(stats.Sockets, socketInfo{
			ID: c.id, ConnectedAt: c.connectedAt, Remote: c.remote, Origin: c.origin, Agent: c.agent,
			Channels: channels, Sent: c.sent.Load(),
		})
	}

	stats.History = historyInfo{
		End:         time.Now(),
		Connections: make([]int, len(h.history)),
		Published:   make([]int64, len(h.history)),
		Delivered:   make([]int64, len(h.history)),
	}

	for i, point := range h.history {
		stats.History.Connections[i] = point.connections
		stats.History.Published[i] = point.published
		stats.History.Delivered[i] = point.delivered
	}

	return stats
}

// clientIP returns the browser's address. Behind a proxy on this same machine (Apache or nginx)
// the connection arrives from 127.0.0.1 and the real address comes in X-Forwarded-For.
//
// The header is trusted only when the connection comes from this machine, and the last address
// is used: it is the one the proxy wrote. Earlier ones can be forged from the browser.
func clientIP(r *http.Request) string {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remote = r.RemoteAddr
	}

	if ip := net.ParseIP(remote); ip == nil || !ip.IsLoopback() {
		return remote
	}

	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	last := strings.TrimSpace(forwarded[len(forwarded)-1])

	if net.ParseIP(last) == nil {
		return remote
	}

	return last
}

func channelType(name string) string {
	switch {
	case strings.HasPrefix(name, "presence-"):
		return "presence"
	case strings.HasPrefix(name, "private-"):
		return "private"
	default:
		return "public"
	}
}

// handleSocket serves one browser connection: handshake, subscriptions, pings and client events.
func (s *server) handleSocket(w http.ResponseWriter, r *http.Request) {
	app := s.apps.byKey[r.PathValue("key")]

	if app != nil && !originAllowed(app.Origins, r.Header.Get("Origin")) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	if app == nil {
		_ = ws.WriteJSON(protocolError("Application does not exist", codeUnknownApp))
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(codeUnknownApp, "unknown app"), time.Now().Add(writeTimeout))
		_ = ws.Close()
		return
	}

	c := app.hub.add(ws, r)
	defer app.hub.remove(c)

	go c.writeLoop()

	c.sendFrame(encodeEvent("pusher:connection_established", "", map[string]any{
		"socket_id":        c.id,
		"activity_timeout": int(activityTimeout.Seconds()),
	}))

	ws.SetReadLimit(maxClientMessage)

	for {
		_ = ws.SetReadDeadline(time.Now().Add(readTimeout))

		_, raw, err := ws.ReadMessage()
		if err != nil {
			return
		}

		var m message
		if json.Unmarshal(raw, &m) != nil {
			continue
		}

		app.handleMessage(c, m)
	}
}

func (app *App) handleMessage(c *Conn, m message) {
	switch {
	case m.Event == "pusher:ping":
		c.sendFrame(encodeEvent("pusher:pong", "", map[string]any{}))

	case m.Event == "pusher:subscribe":
		app.handleSubscribe(c, m.Data)

	case m.Event == "pusher:unsubscribe":
		var data struct {
			Channel string `json:"channel"`
		}

		if json.Unmarshal(m.Data, &data) == nil {
			app.hub.unsubscribe(c, data.Channel)
		}

	case strings.HasPrefix(m.Event, "client-"):
		// Browser-to-browser events are only allowed on authorized channels the sender is subscribed to
		authorized := strings.HasPrefix(m.Channel, "private-") || strings.HasPrefix(m.Channel, "presence-")

		if !authorized || !app.hub.isSubscribed(c, m.Channel) {
			c.sendError("Client events are only allowed on subscribed private or presence channels", codeClientEvents)
			return
		}

		app.hub.deliver(m.Channel, message{Event: m.Event, Channel: m.Channel, Data: m.Data}, c.id, "client")
	}
}

func (app *App) handleSubscribe(c *Conn, raw json.RawMessage) {
	var data struct {
		Channel     string `json:"channel"`
		Auth        string `json:"auth"`
		ChannelData string `json:"channel_data"`
	}

	if json.Unmarshal(raw, &data) != nil || data.Channel == "" || len(data.Channel) > 200 {
		return
	}

	switch {
	case strings.HasPrefix(data.Channel, "private-encrypted-"):
		c.sendError("Encrypted channels are not supported yet", codeNotSupported)

	case strings.HasPrefix(data.Channel, "presence-"):
		userID, info, ok := parseChannelData(data.ChannelData)

		if !ok || !validChannelAuth(app, c.id, data.Channel, data.ChannelData, data.Auth) {
			log.Printf("[%s] subscription to %s rejected: invalid signature", app.Name, data.Channel)
			app.hub.rejectedSubs.Add(1)
			app.hub.reject(data.Channel, "invalid signature", "browser")
			c.sendError("Invalid Signature", codeUnauthorized)
			return
		}

		app.hub.joinPresence(c, data.Channel, userID, info)

	case strings.HasPrefix(data.Channel, "private-"):
		if !validChannelAuth(app, c.id, data.Channel, "", data.Auth) {
			log.Printf("[%s] subscription to %s rejected: invalid signature", app.Name, data.Channel)
			app.hub.rejectedSubs.Add(1)
			app.hub.reject(data.Channel, "invalid signature", "browser")
			c.sendError("Invalid Signature", codeUnauthorized)
			return
		}

		app.hub.subscribe(c, data.Channel)

	default:
		app.hub.subscribe(c, data.Channel)
	}
}

// parseChannelData extracts the user from the data the backend signs for a presence channel.
// The identifier may arrive as a number or as a string.
func parseChannelData(raw string) (userID string, info json.RawMessage, ok bool) {
	var data struct {
		UserID   json.RawMessage `json:"user_id"`
		UserInfo json.RawMessage `json:"user_info"`
	}

	if json.Unmarshal([]byte(raw), &data) != nil {
		return "", nil, false
	}

	userID = strings.Trim(string(data.UserID), `"`)
	if userID == "" || userID == "null" {
		return "", nil, false
	}

	if len(data.UserInfo) == 0 {
		data.UserInfo = json.RawMessage("{}")
	}

	return userID, data.UserInfo, true
}

// encodeEvent builds an event sent by the server itself; the protocol carries `data` as a JSON string.
func encodeEvent(event, channel string, data any) []byte {
	inner, _ := json.Marshal(data)
	payload, _ := json.Marshal(string(inner))
	frame, _ := json.Marshal(message{Event: event, Channel: channel, Data: payload})

	return frame
}

// sendFrame queues a frame for the connection without blocking. If the queue is full the browser
// cannot keep up and is disconnected so it does not hold the others back. It must not be called
// after Hub.remove.
func (c *Conn) sendFrame(frame []byte) bool {
	select {
	case c.send <- frame:
		return true
	default:
		c.hub.droppedSlow.Add(1)
		_ = c.ws.Close()
		return false
	}
}

func (c *Conn) sendError(text string, code int) {
	frame, _ := json.Marshal(protocolError(text, code))

	c.sendFrame(frame)
}

func (c *Conn) writeLoop() {
	defer c.ws.Close()

	for frame := range c.send {
		_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))

		if c.ws.WriteMessage(websocket.TextMessage, frame) != nil {
			return
		}

		c.sent.Add(1)
	}
}

func protocolError(text string, code int) message {
	data, _ := json.Marshal(map[string]any{"message": text, "code": code})

	return message{Event: "pusher:error", Data: data}
}
