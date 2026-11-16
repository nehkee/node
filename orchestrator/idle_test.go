package orchestrator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/livepeer/node/internal/test"
	"github.com/stretchr/testify/require"
)

// idleSession reserves one admitted persistent session for each pricing mode.
func idleSession(t *testing.T, registry *Registry, pricing string) string {
	t.Helper()
	if pricing == "free" {
		sid, _, _, _, err := registry.Reserve("r")
		require.NoError(t, err)
		return sid
	}
	quote := &appQuote{priceInfo: priceInfo{Price: "1", Currency: "wei", Unit: pricing}, registration: registry.appRegistration("r")}
	sid, _, _, _, err := registry.reserveWithPrice("r", "paid-"+pricing, quote, "persistent")
	require.NoError(t, err)
	return sid
}

func activeSession(registry *Registry, sid string) bool {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return registry.sessions[sid] != nil
}

func TestSessionIdleTimeout(t *testing.T) {
	const timeout = time.Minute
	for _, pricing := range []string{"free", "fixed", "seconds"} {
		t.Run(pricing, func(t *testing.T) {
			t.Run("disabled by default", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					registry, server := reviewServer(t, "http://127.0.0.1:1")
					defer server.Close()
					require.NoError(t, registry.AddStatic(StaticRunner{ID: "r", RunnerURL: "http://127.0.0.1:1", App: "test", Capacity: 2}))
					sid := idleSession(t, registry, pricing)
					require.True(t, registry.activateSession("r", sid))
					time.Sleep(24 * time.Hour)
					registry.Expire()
					require.True(t, activeSession(registry, sid))
				})
			})

			t.Run("unused reservation", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					registry, server := reviewServer(t, "http://127.0.0.1:1")
					defer server.Close()
					registry.idleTimeout = timeout
					require.NoError(t, registry.AddStatic(StaticRunner{ID: "r", RunnerURL: "http://127.0.0.1:1", App: "test", Capacity: 2}))
					var events []string
					var mu sync.Mutex
					server.registry.onEvent = func(_, _, event, sid, _ string) {
						mu.Lock()
						defer mu.Unlock()
						events = append(events, event+":"+sid)
						server.emitSessionEvent("r", "r", event, sid, "")
					}
					sid := idleSession(t, registry, pricing)
					if pricing != "free" {
						// The idle clock starts at admission, not challenge reservation.
						time.Sleep(2 * timeout)
						registry.Expire()
						require.True(t, activeSession(registry, sid), "pending reservations are not idle-timed")
						require.True(t, registry.activateSession("r", sid))
					}
					server.mu.Lock()
					server.newChannel("idle-channel", "events", "application/json", "r", "r", sid)
					server.mu.Unlock()
					proxyID, err := registry.addProxy("r", sid, registry.apps["r"].Sessions[sid].Token, mustURL(t, "http://127.0.0.1:1"))
					require.NoError(t, err)
					ctx, ok := registry.sessionContext("r", sid)
					require.True(t, ok)

					time.Sleep(timeout - time.Nanosecond)
					registry.Expire()
					require.True(t, activeSession(registry, sid))
					time.Sleep(time.Nanosecond)
					registry.Expire()
					require.False(t, activeSession(registry, sid), "the deadline is inclusive")
					require.Error(t, ctx.Err(), "release cancels the session context")
					require.NotContains(t, registry.proxies, proxyID)
					require.NotContains(t, server.channels, "idle-channel")
					require.Empty(t, registry.apps["r"].Sessions, "capacity is released")
					require.Contains(t, events, "released:"+sid)
					require.EqualValues(t, 1, test.MetricSum(t, registry.metrics.Registry, "session_ends_total", map[string]string{"reason": "idle_timeout"}))
				})
			})

			t.Run("requests around the deadline", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					registry, server := reviewServer(t, "http://127.0.0.1:1")
					defer server.Close()
					registry.idleTimeout = timeout
					require.NoError(t, registry.AddStatic(StaticRunner{ID: "r", RunnerURL: "http://127.0.0.1:1", App: "test", Capacity: 2}))
					sid := idleSession(t, registry, pricing)
					require.True(t, registry.activateSession("r", sid))

					// Just before the deadline, a request revives the session.
					time.Sleep(timeout - time.Nanosecond)
					_, done, ok := registry.beginRequest("r", sid)
					require.True(t, ok)
					// A request outliving the timeout keeps the session.
					time.Sleep(3 * timeout)
					registry.Expire()
					require.True(t, activeSession(registry, sid))
					done()
					done() // Idempotent completion never double-counts.

					// Inactivity is measured from completion, not arrival.
					time.Sleep(timeout - time.Nanosecond)
					registry.Expire()
					require.True(t, activeSession(registry, sid))
					time.Sleep(time.Nanosecond)
					// A request at the deadline cannot revive the session, even
					// before the periodic sweep runs.
					_, _, ok = registry.beginRequest("r", sid)
					require.False(t, ok)
					require.False(t, activeSession(registry, sid))
				})
			})

			t.Run("overlapping requests", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					registry, server := reviewServer(t, "http://127.0.0.1:1")
					defer server.Close()
					registry.idleTimeout = timeout
					require.NoError(t, registry.AddStatic(StaticRunner{ID: "r", RunnerURL: "http://127.0.0.1:1", App: "test", Capacity: 2}))
					sid := idleSession(t, registry, pricing)
					require.True(t, registry.activateSession("r", sid))
					_, first, ok := registry.beginRequest("r", sid)
					require.True(t, ok)
					time.Sleep(timeout / 2)
					_, second, ok := registry.beginRequest("r", sid)
					require.True(t, ok)
					time.Sleep(timeout)
					first()
					// The second request is still in flight.
					time.Sleep(2 * timeout)
					registry.Expire()
					require.True(t, activeSession(registry, sid))
					second()
					time.Sleep(timeout - time.Nanosecond)
					registry.Expire()
					require.True(t, activeSession(registry, sid))
					time.Sleep(time.Nanosecond)
					registry.Expire()
					require.False(t, activeSession(registry, sid))
				})
			})
		})
	}

	t.Run("single-shot sessions are unaffected", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			registry, server := reviewServer(t, "http://127.0.0.1:1")
			defer server.Close()
			registry.idleTimeout = timeout
			require.NoError(t, registry.AddStatic(StaticRunner{ID: "r", RunnerURL: "http://127.0.0.1:1", App: "test", Mode: "single-shot", Capacity: 1}))
			sid, _, _, _, err := registry.reserveWithPrice("r", "single", nil, "single-shot")
			require.NoError(t, err)
			time.Sleep(2 * timeout)
			registry.Expire()
			require.True(t, activeSession(registry, sid))
		})
	})
}

// TestSessionIdleTimeoutStreams exercises the HTTP paths: a stream longer than
// the idle timeout keeps the session, and expiry follows its completion.
func TestSessionIdleTimeoutStreams(t *testing.T) {
	const timeout = 300 * time.Millisecond
	for _, protocol := range []string{"http", "sse", "websocket", "generated", "trickle"} {
		t.Run(protocol, func(t *testing.T) {
			finish := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch protocol {
				case "websocket":
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					<-finish
				case "sse":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: started\n\n")
					w.(http.Flusher).Flush()
					<-finish
				default:
					// Application errors still count as client activity.
					<-finish
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer upstream.Close()
			registry, app := reviewServer(t, upstream.URL)
			registry.idleTimeout = timeout
			require.NoError(t, registry.AddStatic(StaticRunner{ID: "r", RunnerURL: upstream.URL, App: "test", Proxy: protocol == "generated", Capacity: 1}))
			server := httptest.NewServer(app)
			defer server.Close()
			defer app.Close()
			app.apiOrigin = mustURL(t, server.URL)
			sid, appURL, _, _, err := registry.Reserve("r")
			require.NoError(t, err)
			path := strings.TrimPrefix(appURL, registry.service)
			if protocol == "generated" {
				require.True(t, strings.HasPrefix(path, "/run/"), path)
			}

			clientDone := make(chan struct{})
			switch protocol {
			case "websocket":
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+path, nil)
				require.NoError(t, err)
				go func() { defer close(clientDone); defer conn.Close(); _, _, _ = conn.ReadMessage() }()
			case "trickle":
				app.mu.Lock()
				app.newChannel("idle-trickle", "events", "application/json", "r", "r", sid)
				app.mu.Unlock()
				// A subscriber waits for a segment that has not been published.
				go func() {
					defer close(clientDone)
					response, err := http.Get(server.URL + "/ai/trickle/idle-trickle/0")
					if err == nil {
						_, _ = io.Copy(io.Discard, response.Body)
						response.Body.Close()
					}
				}()
				go func() {
					<-finish
					app.mu.Lock()
					app.publishSessionEvent("idle-trickle", []byte("{}"))
					app.mu.Unlock()
				}()
			default:
				go func() {
					defer close(clientDone)
					response, err := http.Get(server.URL + path)
					if err == nil {
						_, _ = io.Copy(io.Discard, response.Body)
						response.Body.Close()
					}
				}()
			}
			inFlight := func() int {
				registry.mu.Lock()
				defer registry.mu.Unlock()
				if sess := registry.apps["r"].Sessions[sid]; sess != nil {
					return sess.active
				}
				return -1
			}
			require.Eventually(t, func() bool { return inFlight() == 1 }, 3*time.Second, 10*time.Millisecond)
			time.Sleep(3 * timeout)
			registry.Expire()
			require.True(t, activeSession(registry, sid), "an in-flight stream keeps the session")
			close(finish)
			select {
			case <-clientDone:
			case <-time.After(3 * time.Second):
				t.Fatal("client did not finish")
			}
			require.Eventually(t, func() bool { return inFlight() == 0 }, 3*time.Second, 10*time.Millisecond)
			registry.Expire()
			require.True(t, activeSession(registry, sid), "the idle clock restarts at completion")
			require.Eventually(t, func() bool { registry.Expire(); return !activeSession(registry, sid) }, 3*time.Second, 20*time.Millisecond)
			response, err := http.Get(server.URL + path)
			require.NoError(t, err)
			response.Body.Close()
			require.Equal(t, http.StatusNotFound, response.StatusCode)
		})
	}
}

func TestSessionIdleTimeoutIgnoresRunnerTraffic(t *testing.T) {
	registry, app := reviewServer(t, "http://127.0.0.1:1")
	defer app.Close()
	registry.idleTimeout = time.Hour
	registry.runnerService = "https://runner.example"
	app.runnerOrigin = mustURL(t, "https://runner.example")
	require.NoError(t, registry.AddStatic(StaticRunner{ID: "r", RunnerURL: "http://127.0.0.1:1", App: "test", Capacity: 1}))
	sid, _, _, _, err := registry.Reserve("r")
	require.NoError(t, err)
	app.mu.Lock()
	app.newChannel("runner-trickle", "events", "application/json", "r", "r", sid)
	app.mu.Unlock()
	done := make(chan struct{})
	w := httptest.NewRecorder()
	go func() {
		defer close(done)
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://runner.example/ai/trickle/runner-trickle/0", nil))
	}()
	// Runner-origin Trickle requests never count as client activity.
	require.Never(t, func() bool {
		registry.mu.Lock()
		defer registry.mu.Unlock()
		return registry.apps["r"].Sessions[sid].active != 0
	}, 200*time.Millisecond, 10*time.Millisecond)
	app.mu.Lock()
	app.publishSessionEvent("runner-trickle", []byte("{}"))
	app.mu.Unlock()
	<-done
	require.Equal(t, http.StatusOK, w.Code, "the runner request reached Trickle")
}
