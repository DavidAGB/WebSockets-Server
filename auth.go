package main

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	errUnknownKey   = errors.New("unknown application key")
	errExpired      = errors.New("request timestamp expired")
	errBodyChanged  = errors.New("body does not match body_md5")
	errBadSignature = errors.New("invalid signature")
)

// How long a signed request stays valid, the same window Pusher uses.
const signatureWindow = 600 * time.Second

func sign(secret, text string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(text))

	return hex.EncodeToString(mac.Sum(nil))
}

// verifyRequest checks the signature of an HTTP API request the way Pusher's server libraries
// build it: HMAC-SHA256 of "METHOD\npath\nsorted query", leaving auth_signature out.
func verifyRequest(app *App, method, path string, query url.Values, body []byte, now time.Time) error {
	if !hmac.Equal([]byte(query.Get("auth_key")), []byte(app.Key)) {
		return errUnknownKey
	}

	timestamp, err := strconv.ParseInt(query.Get("auth_timestamp"), 10, 64)
	if err != nil || now.Sub(time.Unix(timestamp, 0)).Abs() > signatureWindow {
		return errExpired
	}

	if len(body) > 0 || query.Has("body_md5") {
		sum := md5.Sum(body)

		if !hmac.Equal([]byte(hex.EncodeToString(sum[:])), []byte(query.Get("body_md5"))) {
			return errBodyChanged
		}
	}

	names := make([]string, 0, len(query))
	for name := range query {
		if name != "auth_signature" {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	pairs := make([]string, len(names))
	for i, name := range names {
		pairs[i] = name + "=" + query.Get(name)
	}

	expected := sign(app.Secret, method+"\n"+path+"\n"+strings.Join(pairs, "&"))

	if !hmac.Equal([]byte(expected), []byte(query.Get("auth_signature"))) {
		return errBadSignature
	}

	return nil
}

// channelSignature is the signature that authorizes a socket to join a private or presence
// channel. Presence channels also sign the user data, so the browser cannot alter it.
func channelSignature(app *App, socketID, channel, channelData string) string {
	text := socketID + ":" + channel
	if channelData != "" {
		text += ":" + channelData
	}

	return sign(app.Secret, text)
}

// validChannelAuth checks the "key:signature" token the backend's auth endpoint hands out
// (/broadcasting/auth in Laravel).
func validChannelAuth(app *App, socketID, channel, channelData, auth string) bool {
	key, signature, found := strings.Cut(auth, ":")
	if !found || !hmac.Equal([]byte(key), []byte(app.Key)) {
		return false
	}

	return hmac.Equal([]byte(channelSignature(app, socketID, channel, channelData)), []byte(signature))
}
