package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// testServer starts the real routes with two applications, the way -config would.
func testServer(t *testing.T) (*httptest.Server, *registry) {
	t.Helper()

	config := filepath.Join(t.TempDir(), "apps.json")
	body := `{"apps":[
		{"id":"100","key":"shop-key","secret":"shop-secret","name":"shop","origins":["https://*.shop.test"]},
		{"id":"200","key":"blog-key","secret":"blog-secret","name":"blog"}
	]}`

	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	apps, err := loadApps(config, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(newServer(apps, "test").routes())
	t.Cleanup(ts.Close)

	return ts, apps
}

type frame struct {
	Event   string          `json:"event"`
	Channel string          `json:"channel"`
	Data    json.RawMessage `json:"data"`
}

// client is a browser as the server sees it: it connects with an app key and reads frames.
type client struct {
	t        *testing.T
	ws       *websocket.Conn
	socketID string
}

func connect(t *testing.T, ts *httptest.Server, key, origin string) *client {
	t.Helper()

	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}

	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/app/"+key, header)
	if err != nil {
		t.Fatalf("connect with key %q: %v", key, err)
	}
	t.Cleanup(func() { ws.Close() })

	c := &client{t: t, ws: ws}

	first := c.read()
	if first.Event != "pusher:connection_established" {
		t.Fatalf("expected the handshake, got %s %s", first.Event, first.Data)
	}

	var data struct {
		SocketID string `json:"socket_id"`
	}
	c.decode(first.Data, &data)
	c.socketID = data.SocketID

	return c
}

func (c *client) read() frame {
	c.t.Helper()

	_ = c.ws.SetReadDeadline(time.Now().Add(2 * time.Second))

	var f frame
	if err := c.ws.ReadJSON(&f); err != nil {
		c.t.Fatalf("read: %v", err)
	}

	return f
}

// silent reports whether nothing arrives for a short while.
func (c *client) silent() bool {
	_ = c.ws.SetReadDeadline(time.Now().Add(150 * time.Millisecond))

	var f frame

	return c.ws.ReadJSON(&f) != nil
}

// decode reads a `data` field, which the protocol sends as a JSON string holding JSON.
func (c *client) decode(raw json.RawMessage, out any) {
	c.t.Helper()

	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		c.t.Fatalf("data is not a string: %s", raw)
	}

	if err := json.Unmarshal([]byte(text), out); err != nil {
		c.t.Fatalf("data does not hold JSON: %s", text)
	}
}

func (c *client) subscribe(channel, auth, channelData string) frame {
	c.t.Helper()

	data := map[string]string{"channel": channel}
	if auth != "" {
		data["auth"] = auth
	}
	if channelData != "" {
		data["channel_data"] = channelData
	}

	if err := c.ws.WriteJSON(map[string]any{"event": "pusher:subscribe", "data": data}); err != nil {
		c.t.Fatal(err)
	}

	return c.read()
}

// publish sends an event the way a backend does, signed with the given secret.
func publish(t *testing.T, ts *httptest.Server, app *App, secret, channel, event, payload string) int {
	t.Helper()

	body, _ := json.Marshal(map[string]any{"name": event, "data": payload, "channels": []string{channel}})
	sum := md5.Sum(body)
	path := "/apps/" + app.ID + "/events"
	query := fmt.Sprintf("auth_key=%s&auth_timestamp=%d&auth_version=1.0&body_md5=%s", app.Key, time.Now().Unix(), hex.EncodeToString(sum[:]))
	signature := sign(secret, "POST\n"+path+"\n"+query)

	res, err := http.Post(ts.URL+path+"?"+query+"&auth_signature="+signature, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	return res.StatusCode
}

func TestEventsStayInsideTheirApplication(t *testing.T) {
	ts, apps := testServer(t)
	shop, blog := apps.byID["100"], apps.byID["200"]

	inShop := connect(t, ts, shop.Key, "https://store.shop.test")
	inBlog := connect(t, ts, blog.Key, "")

	if f := inShop.subscribe("news", "", ""); f.Event != "pusher_internal:subscription_succeeded" {
		t.Fatalf("shop subscription: %s", f.Event)
	}
	if f := inBlog.subscribe("news", "", ""); f.Event != "pusher_internal:subscription_succeeded" {
		t.Fatalf("blog subscription: %s", f.Event)
	}

	if status := publish(t, ts, shop, shop.Secret, "news", "OrderPaid", `{"id":7}`); status != http.StatusOK {
		t.Fatalf("publish to shop: %d", status)
	}

	got := inShop.read()
	if got.Event != "OrderPaid" || got.Channel != "news" {
		t.Fatalf("shop client got %s on %s", got.Event, got.Channel)
	}

	var payload struct {
		ID int `json:"id"`
	}
	inShop.decode(got.Data, &payload)
	if payload.ID != 7 {
		t.Errorf("payload id = %d", payload.ID)
	}

	if !inBlog.silent() {
		t.Error("a client of another application received the event")
	}

	// One application's secret must not publish into another
	if status := publish(t, ts, blog, shop.Secret, "news", "Forged", `{}`); status != http.StatusUnauthorized {
		t.Errorf("publishing to blog with shop's secret: %d", status)
	}
	if !inBlog.silent() {
		t.Error("a forged event was delivered")
	}

	if got := shop.hub.connectionCount() + blog.hub.connectionCount(); got != 2 {
		t.Errorf("connections = %d", got)
	}
}

func TestPrivateAndPresenceChannels(t *testing.T) {
	ts, apps := testServer(t)
	blog := apps.byID["200"]

	alice := connect(t, ts, blog.Key, "")

	if f := alice.subscribe("private-orders", blog.Key+":forged", ""); f.Event != "pusher:error" {
		t.Errorf("a forged private authorization was accepted: %s", f.Event)
	}

	auth := blog.Key + ":" + channelSignature(blog, alice.socketID, "private-orders", "")
	if f := alice.subscribe("private-orders", auth, ""); f.Event != "pusher_internal:subscription_succeeded" {
		t.Fatalf("private subscription: %s %s", f.Event, f.Data)
	}

	// Presence: the first member sees itself, the second sees both and the first is told
	member := func(c *client, id, name string) frame {
		data := fmt.Sprintf(`{"user_id":%q,"user_info":{"name":%q}}`, id, name)

		return c.subscribe("presence-room", blog.Key+":"+channelSignature(blog, c.socketID, "presence-room", data), data)
	}

	var roster struct {
		Presence struct {
			IDs   []string `json:"ids"`
			Count int      `json:"count"`
		} `json:"presence"`
	}

	alice.decode(member(alice, "1", "Alice").Data, &roster)
	if roster.Presence.Count != 1 {
		t.Fatalf("first member sees %d members", roster.Presence.Count)
	}

	bob := connect(t, ts, blog.Key, "")
	bob.decode(member(bob, "2", "Bob").Data, &roster)
	if roster.Presence.Count != 2 {
		t.Fatalf("second member sees %d members", roster.Presence.Count)
	}

	if f := alice.read(); f.Event != "pusher_internal:member_added" {
		t.Errorf("alice expected member_added, got %s", f.Event)
	}

	// A client event reaches the others but not its sender
	if err := bob.ws.WriteJSON(map[string]any{"event": "client-typing", "channel": "presence-room", "data": map[string]string{"name": "Bob"}}); err != nil {
		t.Fatal(err)
	}
	if f := alice.read(); f.Event != "client-typing" {
		t.Errorf("alice expected client-typing, got %s", f.Event)
	}
	if !bob.silent() {
		t.Error("the sender received its own client event")
	}

	bob.ws.Close()
	if f := alice.read(); f.Event != "pusher_internal:member_removed" {
		t.Errorf("alice expected member_removed, got %s", f.Event)
	}
}

func TestOriginsAndUnknownKeys(t *testing.T) {
	ts, apps := testServer(t)
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/app/"

	// shop only accepts its own subdomains; blog declares no origins and accepts any
	_, res, err := websocket.DefaultDialer.Dial(url+apps.byID["100"].Key, http.Header{"Origin": {"https://evil.test"}})
	if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("a foreign origin was not refused with 403 (err %v)", err)
	}

	connect(t, ts, apps.byID["200"].Key, "https://evil.test")

	ws, _, err := websocket.DefaultDialer.Dial(url+"no-such-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	var f frame
	if err := ws.ReadJSON(&f); err != nil || f.Event != "pusher:error" || !strings.Contains(string(f.Data), "4001") {
		t.Errorf("an unknown key should get error 4001, got %s %s (%v)", f.Event, f.Data, err)
	}
}

func TestPanelShowsEachApplication(t *testing.T) {
	ts, apps := testServer(t)

	connect(t, ts, apps.byID["200"].Key, "")

	var stats statsSnapshot

	res, err := http.Get(ts.URL + "/stats?app=200")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if err := json.NewDecoder(res.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}

	if stats.App.Name != "blog" || stats.Connections != 1 || len(stats.Apps) != 2 {
		t.Errorf("panel for blog: app %q, %d connections, %d apps listed", stats.App.Name, stats.Connections, len(stats.Apps))
	}

	// A request relayed by a proxy is not local: the panel must not answer it
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/stats", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")

	proxied, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	proxied.Body.Close()

	if proxied.StatusCode != http.StatusNotFound {
		t.Errorf("the panel answered a proxied request with %d", proxied.StatusCode)
	}
}

func TestLoadAppsRejectsBadConfigs(t *testing.T) {
	write := func(body string) string {
		path := filepath.Join(t.TempDir(), "apps.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}

	for name, body := range map[string]string{
		"no apps":        `{"apps":[]}`,
		"missing secret": `{"apps":[{"id":"1","key":"k"}]}`,
		"repeated id":    `{"apps":[{"id":"1","key":"a","secret":"s"},{"id":"1","key":"b","secret":"s"}]}`,
		"repeated key":   `{"apps":[{"id":"1","key":"a","secret":"s"},{"id":"2","key":"a","secret":"s"}]}`,
		"not json":       `apps:`,
	} {
		if _, err := loadApps(write(body), "", nil); err == nil {
			t.Errorf("%s: the config was accepted", name)
		}
	}

	// Without a config file, a single app comes from a .env file such as Laravel's
	env := filepath.Join(t.TempDir(), ".env")
	_ = os.WriteFile(env, []byte("APP_NAME=x\nPUSHER_APP_ID=9\nPUSHER_APP_KEY=\"quoted-key\"\nPUSHER_APP_SECRET='s3'\n"), 0o600)

	apps, err := loadApps("", env, []string{"https://*.example.com"})
	if err != nil {
		t.Fatal(err)
	}

	app := apps.list[0]
	if app.ID != "9" || app.Key != "quoted-key" || app.Secret != "s3" || len(app.Origins) != 1 {
		t.Errorf("app read from .env: %+v", app)
	}
}
