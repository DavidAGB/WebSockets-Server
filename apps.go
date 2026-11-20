package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// App is one application served by this process: its Pusher credentials, the origins allowed
// to connect and its own hub, so channels and statistics never mix between applications.
type App struct {
	ID      string   `json:"id"`
	Key     string   `json:"key"`
	Secret  string   `json:"secret"`
	Name    string   `json:"name"`
	Origins []string `json:"origins"`

	hub *Hub
}

// registry holds the applications by the two identifiers the protocol uses: browsers connect
// with the key, backends publish with the id.
type registry struct {
	list  []*App
	byID  map[string]*App
	byKey map[string]*App
}

var errMissingCredentials = errors.New("no application configured: set PUSHER_APP_ID, PUSHER_APP_KEY and PUSHER_APP_SECRET (environment or -env file), or pass -config with a list of apps")

// loadApps reads the applications from a JSON config file, or a single application from the
// environment and an optional .env file. defaultOrigins applies to apps that declare none.
func loadApps(configFile, envFile string, defaultOrigins []string) (*registry, error) {
	var apps []*App

	if configFile != "" {
		raw, err := os.ReadFile(configFile)
		if err != nil {
			return nil, err
		}

		var config struct {
			Apps []*App `json:"apps"`
		}

		if err := json.Unmarshal(raw, &config); err != nil {
			return nil, fmt.Errorf("%s: %w", configFile, err)
		}

		apps = config.Apps
	} else {
		app, err := appFromEnv(envFile)
		if err != nil {
			return nil, err
		}

		apps = []*App{app}
	}

	if len(apps) == 0 {
		return nil, errMissingCredentials
	}

	r := &registry{byID: map[string]*App{}, byKey: map[string]*App{}}

	for _, app := range apps {
		if app == nil || app.ID == "" || app.Key == "" || app.Secret == "" {
			return nil, errors.New("every app needs an id, a key and a secret")
		}

		if r.byID[app.ID] != nil || r.byKey[app.Key] != nil {
			return nil, fmt.Errorf("app %q repeats an id or a key", app.ID)
		}

		if app.Name == "" {
			app.Name = app.ID
		}

		if len(app.Origins) == 0 {
			app.Origins = defaultOrigins
		}

		app.hub = newHub()
		go app.hub.runSampler()

		r.list = append(r.list, app)
		r.byID[app.ID] = app
		r.byKey[app.Key] = app
	}

	return r, nil
}

// appFromEnv takes the credentials from the environment and, when given, from a .env file
// such as Laravel's. The environment wins over the file.
func appFromEnv(envFile string) (*App, error) {
	values := map[string]string{}

	if envFile != "" {
		file, err := os.Open(envFile)
		if err != nil {
			return nil, err
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			name, value, found := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
			if found && strings.HasPrefix(name, "PUSHER_APP_") {
				values[name] = strings.Trim(strings.TrimSpace(value), `"'`)
			}
		}
	}

	read := func(name string) string {
		if value := os.Getenv(name); value != "" {
			return value
		}

		return values[name]
	}

	app := &App{ID: read("PUSHER_APP_ID"), Key: read("PUSHER_APP_KEY"), Secret: read("PUSHER_APP_SECRET")}

	if app.ID == "" || app.Key == "" || app.Secret == "" {
		return nil, errMissingCredentials
	}

	return app, nil
}

// splitOrigins turns the comma-separated -origins flag into a list.
func splitOrigins(value string) []string {
	var origins []string

	for _, origin := range strings.Split(value, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			origins = append(origins, origin)
		}
	}

	return origins
}

// originAllowed reports whether a browser origin matches one of the patterns. A pattern is an
// exact origin ("https://app.example.com") or a subdomain wildcard ("https://*.example.com").
// An empty list allows every origin.
func originAllowed(patterns []string, origin string) bool {
	if len(patterns) == 0 {
		return true
	}

	for _, pattern := range patterns {
		scheme, host, wildcard := strings.Cut(pattern, "://*.")

		switch {
		case !wildcard:
			if origin == pattern {
				return true
			}
		case strings.HasPrefix(origin, scheme+"://") && strings.HasSuffix(origin, "."+host):
			// Only the subdomain may sit between the scheme and the domain: no slashes, no second "://"
			subdomain := strings.TrimSuffix(strings.TrimPrefix(origin, scheme+"://"), "."+host)

			if subdomain != "" && !strings.ContainsAny(subdomain, "/:@?#") {
				return true
			}
		}
	}

	return false
}
