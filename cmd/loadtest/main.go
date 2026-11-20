// loadtest opens many connections to the server, subscribes them to a public channel and measures
// how long each event takes to reach all of them. It uses the local panel endpoints (/stats and
// /debug/send), so it must run on the same machine as the server.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type frame struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

func main() {
	host := flag.String("host", "127.0.0.1:6001", "server to test")
	conns := flag.Int("conns", 1000, "simultaneous connections")
	events := flag.Int("events", 20, "events to publish")
	appID := flag.String("app", "", "application id (defaults to the server's first app)")
	flag.Parse()

	var stats struct {
		AppKey string `json:"appKey"`
	}

	query := ""
	if *appID != "" {
		query = "?app=" + url.QueryEscape(*appID)
	}

	res, err := http.Get("http://" + *host + "/stats" + query)
	if err != nil {
		log.Fatal(err)
	}
	_ = json.NewDecoder(res.Body).Decode(&stats)
	res.Body.Close()

	const channel = "load"

	var ready, received sync.WaitGroup
	var failed atomic.Int64
	latencies := make(chan time.Duration, *conns**events)

	start := time.Now()
	ready.Add(*conns)

	for i := 0; i < *conns; i++ {
		go func() {
			ws, _, err := websocket.DefaultDialer.Dial("ws://"+*host+"/app/"+stats.AppKey+"?protocol=7&client=loadtest", nil)
			if err != nil {
				failed.Add(1)
				ready.Done()
				return
			}
			defer ws.Close()

			subscribed := false

			for {
				var f frame
				if ws.ReadJSON(&f) != nil {
					if !subscribed {
						failed.Add(1)
						ready.Done()
					}
					return
				}

				switch {
				case f.Event == "pusher:connection_established":
					_ = ws.WriteJSON(map[string]any{"event": "pusher:subscribe", "data": map[string]string{"channel": channel}})
				case f.Event == "pusher_internal:subscription_succeeded":
					subscribed = true
					ready.Done()
				case f.Event == "Load":
					var payload struct {
						SentAt int64 `json:"sentAt"`
					}
					_ = json.Unmarshal([]byte(f.Data), &payload)
					latencies <- time.Since(time.Unix(0, payload.SentAt))
					received.Done()
				}
			}
		}()
	}

	ready.Wait()

	connected := *conns - int(failed.Load())
	fmt.Printf("%d of %d connections subscribed in %s\n", connected, *conns, time.Since(start).Round(time.Millisecond))

	if connected == 0 {
		return
	}

	received.Add(connected * *events)
	publishStart := time.Now()

	for i := 0; i < *events; i++ {
		data, _ := json.Marshal(map[string]any{"sentAt": time.Now().UnixNano(), "padding": strings.Repeat("x", 300)})
		body, _ := json.Marshal(map[string]string{"name": "Load", "channel": channel, "data": string(data)})

		res, err := http.Post("http://"+*host+"/debug/send"+query, "application/json", bytes.NewReader(body))
		if err != nil {
			log.Fatal(err)
		}
		res.Body.Close()
	}

	done := make(chan struct{})
	go func() { received.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		fmt.Println("WARNING: not every message arrived within 30 s")
	}

	total := time.Since(publishStart)
	close(latencies)

	var all []time.Duration
	for l := range latencies {
		all = append(all, l)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	if len(all) == 0 {
		fmt.Println("No message was received")
		return
	}

	pick := func(p float64) time.Duration { return all[int(float64(len(all)-1)*p)].Round(10 * time.Microsecond) }

	fmt.Printf("%d messages delivered of %d expected in %s (%.0f messages/s)\n", len(all), connected**events, total.Round(time.Millisecond), float64(len(all))/total.Seconds())
	fmt.Printf("time from publish to delivery: median %s · p95 %s · p99 %s · max %s\n", pick(0.5), pick(0.95), pick(0.99), all[len(all)-1].Round(10*time.Microsecond))
}
