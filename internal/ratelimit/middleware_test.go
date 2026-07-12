package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
)

// fakeSettingsStore is a map-backed SettingsStore for tests.
type fakeSettingsStore struct {
	values map[string]string
}

func newFakeSettingsStore() *fakeSettingsStore {
	return &fakeSettingsStore{values: map[string]string{}}
}

func (s *fakeSettingsStore) Get(_ context.Context, key string) (string, error) {
	return s.values[key], nil
}

func (s *fakeSettingsStore) Set(_ context.Context, key, value string) error {
	s.values[key] = value
	return nil
}

func (s *fakeSettingsStore) GetAll(_ context.Context) (map[string]string, error) {
	out := make(map[string]string, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out, nil
}

func newTestMiddleware(t *testing.T, settings map[string]string) *Middleware {
	t.Helper()
	store := newFakeSettingsStore()
	for k, v := range settings {
		store.values[k] = v
	}
	perKey := NewMemoryLimiter()
	global := NewMemoryLimiter()
	t.Cleanup(perKey.Close)
	t.Cleanup(global.Close)
	mw := NewMiddleware(perKey, global, store, true)
	if err := mw.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return mw
}

func doRequest(mw *Middleware, claims *auth.Claims, path string) *httptest.ResponseRecorder {
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if claims != nil {
		req = req.WithContext(apimw.SetClaims(req.Context(), claims))
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func sessionClaims(userID int) *auth.Claims {
	return &auth.Claims{UserID: userID, TokenType: auth.TokenTypeAccess, SessionID: "sess"}
}

func TestSessionLimitExceededReturns429(t *testing.T) {
	mw := newTestMiddleware(t, map[string]string{
		"ratelimit.session.requests_per_second": "100",
		"ratelimit.session.requests_per_minute": "6000",
		"ratelimit.session.burst":               "3",
	})

	var got429 bool
	for i := 0; i < 10; i++ {
		rec := doRequest(mw, sessionClaims(7), "/api/v1/recommendations/similar/movie-1")
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("429 response missing Retry-After header")
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			break
		}
	}
	if !got429 {
		t.Fatal("session limit never returned 429 after burst exhausted")
	}
}

func TestSessionLimitDoesNotSetRateLimitHeadersOnAllowed(t *testing.T) {
	mw := newTestMiddleware(t, nil)
	rec := doRequest(mw, sessionClaims(7), "/api/v1/catalog/items/movie-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("X-RateLimit-Limit") != "" {
		t.Fatal("X-RateLimit-Limit set on session-authenticated response; that contract is API-key-only")
	}
}

func TestSessionLimitSkipsPlaybackExemptPaths(t *testing.T) {
	mw := newTestMiddleware(t, map[string]string{
		"ratelimit.session.requests_per_second": "100",
		"ratelimit.session.requests_per_minute": "6000",
		"ratelimit.session.burst":               "1",
	})

	for i := 0; i < 20; i++ {
		for _, path := range []string{
			"/api/v1/playback/transcode/abc/segment/seg_00001.ts",
			"/api/v1/stream/abc/direct",
		} {
			rec := doRequest(mw, sessionClaims(7), path)
			if rec.Code != http.StatusOK {
				t.Fatalf("request %d to %s: status = %d, want 200", i, path, rec.Code)
			}
		}
	}
}

func TestSessionLimitBucketsAreIndependentPerUser(t *testing.T) {
	mw := newTestMiddleware(t, map[string]string{
		"ratelimit.session.requests_per_second": "100",
		"ratelimit.session.requests_per_minute": "6000",
		"ratelimit.session.burst":               "2",
	})

	// Exhaust user 1's bucket.
	var user1Limited bool
	for i := 0; i < 10; i++ {
		if doRequest(mw, sessionClaims(1), "/api/v1/catalog/items/x").Code == http.StatusTooManyRequests {
			user1Limited = true
			break
		}
	}
	if !user1Limited {
		t.Fatal("user 1 never hit the session limit")
	}
	if rec := doRequest(mw, sessionClaims(2), "/api/v1/catalog/items/x"); rec.Code != http.StatusOK {
		t.Fatalf("user 2 status = %d, want 200 (buckets must be independent)", rec.Code)
	}
}

func TestAPIKeyClaimsTakeTierPathNotSessionLimit(t *testing.T) {
	// Session limit is tight; API-key standard tier is generous. An API-key
	// request must be limited by its tier, not the session bucket.
	mw := newTestMiddleware(t, map[string]string{
		"ratelimit.session.requests_per_second": "100",
		"ratelimit.session.requests_per_minute": "6000",
		"ratelimit.session.burst":               "1",
	})
	claims := &auth.Claims{UserID: 7, TokenType: auth.TokenTypeAPIKey, APIKeyID: 42}

	for i := 0; i < 5; i++ {
		rec := doRequest(mw, claims, "/api/v1/catalog/items/x")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
		if rec.Header().Get("X-RateLimit-Limit") == "" {
			t.Fatal("API-key response missing X-RateLimit-Limit header")
		}
	}
}

func TestNilClaimsSkipUserCheck(t *testing.T) {
	mw := newTestMiddleware(t, map[string]string{
		"ratelimit.session.requests_per_second": "100",
		"ratelimit.session.requests_per_minute": "6000",
		"ratelimit.session.burst":               "1",
	})
	for i := 0; i < 10; i++ {
		if rec := doRequest(mw, nil, "/api/v1/catalog/items/x"); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
	}
}

func TestReloadPicksUpSessionConfig(t *testing.T) {
	store := newFakeSettingsStore()
	perKey := NewMemoryLimiter()
	global := NewMemoryLimiter()
	t.Cleanup(perKey.Close)
	t.Cleanup(global.Close)
	mw := NewMiddleware(perKey, global, store, true)
	if err := mw.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	defaults := DefaultConfig()
	mw.mu.RLock()
	got := mw.cfg.Session
	mw.mu.RUnlock()
	if got != defaults.Session {
		t.Fatalf("Session config = %+v, want defaults %+v", got, defaults.Session)
	}

	store.values["ratelimit.session.requests_per_second"] = "5"
	store.values["ratelimit.session.requests_per_minute"] = "50"
	store.values["ratelimit.session.burst"] = strconv.Itoa(9)
	if err := mw.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	mw.mu.RLock()
	got = mw.cfg.Session
	mw.mu.RUnlock()
	want := TierConfig{RequestsPerSecond: 5, RequestsPerMinute: 50, Burst: 9}
	if got != want {
		t.Fatalf("Session config after reload = %+v, want %+v", got, want)
	}
}

func TestSaveAndLoadConfigRoundTripsSession(t *testing.T) {
	store := newFakeSettingsStore()
	ctx := context.Background()

	cfg := DefaultConfig()
	cfg.Session = TierConfig{RequestsPerSecond: 12, RequestsPerMinute: 240, Burst: 24}
	if err := SaveConfig(ctx, store, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	loaded, err := LoadConfig(ctx, store)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.Session != cfg.Session {
		t.Fatalf("Session = %+v, want %+v", loaded.Session, cfg.Session)
	}
}
