package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

// The vectors come from pusher-php-server, the library Laravel signs with: if these tests pass,
// the server accepts what a real backend sends and rejects anything tampered with.
type vectors struct {
	Key, Secret, AppID string
	Timestamp          int64
	Method, Path       string
	Body, Query        string
	SocketID, Channel  string
	Auth               string
	PresenceChannel    string
	ChannelData        string
	PresenceAuth       string
}

func loadVectors(t *testing.T) (vectors, *App) {
	t.Helper()

	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}

	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}

	return v, &App{ID: v.AppID, Key: v.Key, Secret: v.Secret}
}

func TestVerifyRequest(t *testing.T) {
	v, app := loadVectors(t)
	now := time.Unix(v.Timestamp, 0).Add(time.Minute)

	query, err := url.ParseQuery(v.Query)
	if err != nil {
		t.Fatal(err)
	}

	if err := verifyRequest(app, v.Method, v.Path, query, []byte(v.Body), now); err != nil {
		t.Fatalf("a request signed by the backend was rejected: %v", err)
	}

	cases := map[string]struct {
		app   *App
		path  string
		body  string
		now   time.Time
		want  error
		query func(url.Values)
	}{
		"other secret":    {app: &App{ID: app.ID, Key: app.Key, Secret: "other"}, path: v.Path, body: v.Body, now: now, want: errBadSignature},
		"other key":       {app: &App{ID: app.ID, Key: "other", Secret: app.Secret}, path: v.Path, body: v.Body, now: now, want: errUnknownKey},
		"tampered body":   {app: app, path: v.Path, body: v.Body + " ", now: now, want: errBodyChanged},
		"other path":      {app: app, path: "/apps/42/batch_events", body: v.Body, now: now, want: errBadSignature},
		"old request":     {app: app, path: v.Path, body: v.Body, now: now.Add(time.Hour), want: errExpired},
		"added parameter": {app: app, path: v.Path, body: v.Body, now: now, want: errBadSignature, query: func(q url.Values) { q.Set("extra", "1") }},
	}

	for name, c := range cases {
		q, _ := url.ParseQuery(v.Query)
		if c.query != nil {
			c.query(q)
		}

		if got := verifyRequest(c.app, v.Method, c.path, q, []byte(c.body), c.now); got != c.want {
			t.Errorf("%s: want %v, got %v", name, c.want, got)
		}
	}
}

func TestValidChannelAuth(t *testing.T) {
	v, app := loadVectors(t)

	if !validChannelAuth(app, v.SocketID, v.Channel, "", v.Auth) {
		t.Fatal("an authorization issued by the backend was rejected")
	}

	rejected := map[string][3]string{
		"other socket":    {"999.1", v.Channel, v.Auth},
		"other channel":   {v.SocketID, "private-orders.2", v.Auth},
		"no signature":    {v.SocketID, v.Channel, v.Key},
		"empty signature": {v.SocketID, v.Channel, v.Key + ":"},
		"other key":       {v.SocketID, v.Channel, "other:" + v.Auth[len(v.Key)+1:]},
		"missing auth":    {v.SocketID, v.Channel, ""},
	}

	for name, c := range rejected {
		if validChannelAuth(app, c[0], c[1], "", c[2]) {
			t.Errorf("%s: an authorization that should be rejected was accepted", name)
		}
	}
}

func TestValidPresenceAuth(t *testing.T) {
	v, app := loadVectors(t)

	if !validChannelAuth(app, v.SocketID, v.PresenceChannel, v.ChannelData, v.PresenceAuth) {
		t.Fatal("a presence authorization issued by the backend was rejected")
	}

	userID, info, ok := parseChannelData(v.ChannelData)
	if !ok || userID != "u-7" || !json.Valid(info) {
		t.Fatalf("could not read the user data: %q %s %v", userID, info, ok)
	}

	// Changing the name or the user invalidates the signature: nobody can impersonate another user
	forged := `{"user_id":"u-1","user_info":{"id":"u-1","name":"Admin"}}`

	if validChannelAuth(app, v.SocketID, v.PresenceChannel, forged, v.PresenceAuth) {
		t.Error("tampered user data was accepted")
	}

	if validChannelAuth(app, v.SocketID, v.PresenceChannel, "", v.PresenceAuth) {
		t.Error("a presence signature was accepted without the user data")
	}

	for _, raw := range []string{"", "{}", `{"user_id":null}`, `{"user_id":""}`, "not json"} {
		if _, _, ok := parseChannelData(raw); ok {
			t.Errorf("invalid user data was accepted: %q", raw)
		}
	}

	if id, _, ok := parseChannelData(`{"user_id":25}`); !ok || id != "25" {
		t.Errorf("a numeric identifier must be read as text: %q %v", id, ok)
	}
}

func TestOriginAllowed(t *testing.T) {
	patterns := []string{"https://*.example.com", "http://localhost:8080"}

	allowed := []string{"https://app.example.com", "https://tenant-1.example.com", "https://a.b.example.com", "http://localhost:8080"}
	rejected := []string{
		"", "https://example.com", "http://app.example.com", "https://example.com.attacker.com",
		"https://attacker.com/.example.com", "https://attacker.com#.example.com", "https://x@attacker.com:443/.example.com",
		"https://badexample.com", "http://localhost:8081", "https://.example.com",
	}

	for _, origin := range allowed {
		if !originAllowed(patterns, origin) {
			t.Errorf("%q should be allowed", origin)
		}
	}

	for _, origin := range rejected {
		if originAllowed(patterns, origin) {
			t.Errorf("%q should be rejected", origin)
		}
	}

	if !originAllowed(nil, "https://anything.example") {
		t.Error("an empty list must allow every origin")
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct{ name, remote, forwarded, want string }{
		{"behind the local proxy", "127.0.0.1:51000", "190.25.10.7", "190.25.10.7"},
		{"the browser forges an address before the real one", "127.0.0.1:51000", "1.2.3.4, 190.25.10.7", "190.25.10.7"},
		{"local proxy over IPv6", "[::1]:51000", "2800:e2:1::5", "2800:e2:1::5"},
		{"local connection without a proxy", "127.0.0.1:51000", "", "127.0.0.1"},
		{"header that is not an address", "127.0.0.1:51000", "not-an-ip", "127.0.0.1"},
		{"direct outside connection: the header is not trusted", "190.25.10.7:40000", "8.8.8.8", "190.25.10.7"},
	}

	for _, c := range cases {
		r, _ := http.NewRequest(http.MethodGet, "/app/key", nil)
		r.RemoteAddr = c.remote

		if c.forwarded != "" {
			r.Header.Set("X-Forwarded-For", c.forwarded)
		}

		if got := clientIP(r); got != c.want {
			t.Errorf("%s: want %s, got %s", c.name, c.want, got)
		}
	}
}
