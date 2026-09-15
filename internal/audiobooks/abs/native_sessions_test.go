package abs

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

type playStartMediaStore struct {
	noopMediaStore
	item  *models.MediaItem
	files []*models.MediaFile
}

func (s *playStartMediaStore) GetAudiobookByID(_ context.Context, id string, _ catalog.AccessFilter) (*models.MediaItem, error) {
	if s.item != nil && s.item.ContentID == id {
		return s.item, nil
	}
	return nil, nil
}

func (s *playStartMediaStore) GetMediaFiles(_ context.Context, contentID string, _ catalog.AccessFilter) ([]*models.MediaFile, error) {
	if s.item != nil && s.item.ContentID == contentID {
		return s.files, nil
	}
	return nil, nil
}

type recordingPlaybackSessionSyncer struct {
	calls int
}

func (s *recordingPlaybackSessionSyncer) SyncNow(context.Context) error {
	s.calls++
	return nil
}

func TestHandlePlayStartCreatesNativePlaybackSession(t *testing.T) {
	now := time.Now()
	media := &playStartMediaStore{
		item: &models.MediaItem{
			ContentID: "book-1",
			Type:      "audiobook",
			Title:     "Native Session Book",
			UpdatedAt: now,
			AddedAt:   &now,
		},
		files: []*models.MediaFile{{
			ID:         42,
			ContentID:  "book-1",
			FilePath:   "/tmp/book.mp3",
			FileSize:   1024,
			Duration:   3600,
			Bitrate:    128,
			CodecAudio: "mp3",
		}},
	}
	absSessions := &fakePlaybackSessionStore{}
	nativeSessions := playback.NewSessionManager(0, 0)
	syncer := &recordingPlaybackSessionSyncer{}
	progress := &fakeProgressStore{row: &ProgressRow{
		UserID:          "1",
		ProfileID:       "profile-1",
		ContentID:       "book-1",
		CurrentSeconds:  123.5,
		DurationSeconds: 3600,
		UpdatedAt:       now,
	}}
	h := New(Dependencies{
		MediaStore:           media,
		ProgressStore:        progress,
		PlaybackSessionStore: absSessions,
		NativeSessions:       nativeSessions,
		NativeSessionSyncer:  syncer,
	})

	rec := dispatchABSWithParams(
		http.MethodPost,
		"/api/items/book-1/play",
		map[string]string{"libraryItemId": "book-1"},
		nil,
		"1",
		"profile-1",
		h.handlePlayStart,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	sessionID, _ := body["id"].(string)
	if sessionID == "" {
		t.Fatalf("response id is empty: %#v", body["id"])
	}
	native, err := nativeSessions.GetSession(sessionID)
	if err != nil {
		t.Fatalf("native session %q missing: %v", sessionID, err)
	}
	if native.MediaFileID != 42 || native.RequestedMediaFileID != 42 {
		t.Fatalf("native file ids = (%d, %d), want (42, 42)", native.MediaFileID, native.RequestedMediaFileID)
	}
	if !native.DisableProgressPersistence {
		t.Fatalf("native session should disable progress persistence")
	}
	if native.Position != 123.5 {
		t.Fatalf("native position = %v, want 123.5", native.Position)
	}
	if syncer.calls == 0 {
		t.Fatalf("native session syncer was not called")
	}
	absSession, err := absSessions.GetPlaybackSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ABS session %q missing: %v", sessionID, err)
	}
	if absSession.CurrentPositionSeconds != 123.5 {
		t.Fatalf("ABS session position = %v, want 123.5", absSession.CurrentPositionSeconds)
	}
	tracks, _ := body["audioTracks"].([]any)
	if len(tracks) != 1 {
		t.Fatalf("audioTracks length = %d, want 1", len(tracks))
	}
	track, _ := tracks[0].(map[string]any)
	if got, _ := track["contentUrl"].(string); got == "" || !strings.Contains(got, "/abs/public/session/"+sessionID+"/track/1") {
		t.Fatalf("contentUrl = %q, want session-scoped URL", got)
	}
	libraryItem, _ := body["libraryItem"].(map[string]any)
	for _, key := range []string{"mtimeMs", "ctimeMs", "birthtimeMs"} {
		if _, ok := libraryItem[key].(float64); !ok {
			t.Errorf("libraryItem[%q] = %#v, want number", key, libraryItem[key])
		}
	}
}

func TestHandleSessionSyncUpdatesNativePlaybackSession(t *testing.T) {
	media := &playStartMediaStore{
		item: &models.MediaItem{ContentID: "book-1", Type: "audiobook", Title: "Book", UpdatedAt: time.Now()},
	}
	absSessions := &fakePlaybackSessionStore{}
	nativeSessions := playback.NewSessionManager(0, 0)
	native, err := nativeSessions.StartSessionWithFilesContext(context.Background(), 1, "profile-1", 42, 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("start native session: %v", err)
	}
	_ = absSessions.InsertPlaybackSession(context.Background(), ABSPlaybackSession{
		ID:        native.ID,
		UserID:    "1",
		ProfileID: "profile-1",
		ContentID: "book-1",
	})
	syncer := &recordingPlaybackSessionSyncer{}
	h := New(Dependencies{
		MediaStore:           media,
		ProgressStore:        &fakeProgressStore{},
		PlaybackSessionStore: absSessions,
		NativeSessions:       nativeSessions,
		NativeSessionSyncer:  syncer,
	})

	rec := dispatchABSWithParams(
		http.MethodPatch,
		"/api/session/"+native.ID,
		map[string]string{"sid": native.ID},
		[]byte(`{"currentTime":55.25,"timeListening":10}`),
		"1",
		"profile-1",
		h.handleSessionSync,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	updated, err := nativeSessions.GetSession(native.ID)
	if err != nil {
		t.Fatalf("native session missing: %v", err)
	}
	if updated.Position != 55.25 {
		t.Fatalf("native position = %v, want 55.25", updated.Position)
	}
	if updated.IsPaused {
		t.Fatalf("native session should be marked playing")
	}
	if syncer.calls == 0 {
		t.Fatalf("native session syncer was not called")
	}
}

// TestHandlePlayStartCreatesProgressRowOnFirstListen guards against a
// regression where the very first online listen of a book (no existing
// user_watch_progress row) never got one: the session-sync heartbeat is
// deliberately UPDATE-only (PR #169 reverted insert-on-missing there, to
// stop a stray background tick from resurrecting progress the user
// explicitly cleared), so nothing else in the online path created the row —
// the book never surfaced in Continue Listening no matter how long it played.
// Play-start is the right place: it is a one-time, explicit action, not a
// recurring tick, so the resurrection risk the heartbeat guards against
// doesn't apply here.
func TestHandlePlayStartCreatesProgressRowOnFirstListen(t *testing.T) {
	media := &playStartMediaStore{
		item:  &models.MediaItem{ContentID: "book-1", Type: "audiobook", Title: "Book", UpdatedAt: time.Now(), Runtime: 600},
		files: []*models.MediaFile{{ID: 42, ContentID: "book-1", FilePath: "/tmp/book.mp3", Duration: 3600}},
	}
	progress := &fakeProgressStore{} // row == nil: no progress row exists yet
	h := New(Dependencies{
		MediaStore:           media,
		ProgressStore:        progress,
		PlaybackSessionStore: &fakePlaybackSessionStore{},
		NativeSessions:       playback.NewSessionManager(0, 0),
		NativeSessionSyncer:  &recordingPlaybackSessionSyncer{},
	})

	rec := dispatchABSWithParams(
		http.MethodPost,
		"/api/items/book-1/play",
		map[string]string{"libraryItemId": "book-1"},
		nil,
		"1",
		"profile-1",
		h.handlePlayStart,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if progress.upsertCalls != 1 {
		t.Fatalf("upsertCalls = %d, want 1 (row should be created on first play)", progress.upsertCalls)
	}
	if progress.lastUpsert.ContentID != "book-1" || progress.lastUpsert.CurrentSeconds != 0 {
		t.Fatalf("unexpected upsert row: %+v", progress.lastUpsert)
	}
	if progress.lastUpsert.DurationSeconds <= 0 {
		t.Fatalf("upsert row has no duration: %+v", progress.lastUpsert)
	}
}

// TestHandlePlayStartLeavesExistingProgressRowAlone guards the other half of
// the same fix: resuming a book that already has a progress row must not
// touch it via play-start (no accidental reset of position or duration) — the
// heartbeat's own monotonic update remains the only writer from then on.
func TestHandlePlayStartLeavesExistingProgressRowAlone(t *testing.T) {
	media := &playStartMediaStore{
		item:  &models.MediaItem{ContentID: "book-1", Type: "audiobook", Title: "Book", UpdatedAt: time.Now(), Runtime: 600},
		files: []*models.MediaFile{{ID: 42, ContentID: "book-1", FilePath: "/tmp/book.mp3", Duration: 3600}},
	}
	progress := &fakeProgressStore{row: &ProgressRow{
		UserID: "1", ProfileID: "profile-1", ContentID: "book-1", CurrentSeconds: 123.5, DurationSeconds: 3600,
	}}
	h := New(Dependencies{
		MediaStore:           media,
		ProgressStore:        progress,
		PlaybackSessionStore: &fakePlaybackSessionStore{},
		NativeSessions:       playback.NewSessionManager(0, 0),
		NativeSessionSyncer:  &recordingPlaybackSessionSyncer{},
	})

	rec := dispatchABSWithParams(
		http.MethodPost,
		"/api/items/book-1/play",
		map[string]string{"libraryItemId": "book-1"},
		nil,
		"1",
		"profile-1",
		h.handlePlayStart,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if progress.upsertCalls != 0 {
		t.Fatalf("upsertCalls = %d, want 0 (an existing row must not be overwritten by play-start)", progress.upsertCalls)
	}
}

// TestHandleSessionSyncCreatesProgressRowOnFirstOnlineListen covers the case
// handlePlayStart's own fix can't reach: a client (observed: the ABS iOS app)
// that keeps heartbeating an existing, still-open session across a server
// restart without ever calling /play again. Every tick that reaches this
// handler already passed an open-session lookup, so creating the row here is
// trustworthy -- it reflects genuinely active playback, not a stray tick.
func TestHandleSessionSyncCreatesProgressRowOnFirstOnlineListen(t *testing.T) {
	media := &playStartMediaStore{
		item: &models.MediaItem{ContentID: "book-1", Type: "audiobook", Title: "Book", UpdatedAt: time.Now(), Runtime: 600},
	}
	absSessions := &fakePlaybackSessionStore{}
	nativeSessions := playback.NewSessionManager(0, 0)
	native, err := nativeSessions.StartSessionWithFilesContext(context.Background(), 1, "profile-1", 42, 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("start native session: %v", err)
	}
	_ = absSessions.InsertPlaybackSession(context.Background(), ABSPlaybackSession{
		ID:        native.ID,
		UserID:    "1",
		ProfileID: "profile-1",
		ContentID: "book-1",
	})
	progress := &fakeProgressStore{} // row == nil: no progress row exists yet
	h := New(Dependencies{
		MediaStore:           media,
		ProgressStore:        progress,
		PlaybackSessionStore: absSessions,
		NativeSessions:       nativeSessions,
		NativeSessionSyncer:  &recordingPlaybackSessionSyncer{},
	})

	rec := dispatchABSWithParams(
		http.MethodPatch,
		"/api/session/"+native.ID,
		map[string]string{"sid": native.ID},
		[]byte(`{"currentTime":55.25,"timeListening":10}`),
		"1",
		"profile-1",
		h.handleSessionSync,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if progress.upsertCalls != 1 {
		t.Fatalf("upsertCalls = %d, want 1 (row should be created on first sync)", progress.upsertCalls)
	}
	if progress.updateCalls != 0 {
		t.Fatalf("updateCalls = %d, want 0 (UpdateProgressPosition is a no-op with no existing row)", progress.updateCalls)
	}
	if progress.lastUpsert.ContentID != "book-1" || progress.lastUpsert.CurrentSeconds != 55.25 {
		t.Fatalf("unexpected upsert row: %+v", progress.lastUpsert)
	}
	if progress.lastUpsert.DurationSeconds <= 0 {
		t.Fatalf("upsert row has no duration: %+v", progress.lastUpsert)
	}
}

// TestHandleSessionSyncUpdatesExistingProgressRow guards the other half of the
// same fix: once a row exists, the sync heartbeat must keep using
// UpdateProgressPosition (not a full upsert), so it can't un-finish a book or
// overwrite a progress_pct the user set explicitly.
func TestHandleSessionSyncUpdatesExistingProgressRow(t *testing.T) {
	media := &playStartMediaStore{
		item: &models.MediaItem{ContentID: "book-1", Type: "audiobook", Title: "Book", UpdatedAt: time.Now(), Runtime: 600},
	}
	absSessions := &fakePlaybackSessionStore{}
	nativeSessions := playback.NewSessionManager(0, 0)
	native, err := nativeSessions.StartSessionWithFilesContext(context.Background(), 1, "profile-1", 42, 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("start native session: %v", err)
	}
	_ = absSessions.InsertPlaybackSession(context.Background(), ABSPlaybackSession{
		ID:        native.ID,
		UserID:    "1",
		ProfileID: "profile-1",
		ContentID: "book-1",
	})
	progress := &fakeProgressStore{row: &ProgressRow{
		UserID: "1", ProfileID: "profile-1", ContentID: "book-1", CurrentSeconds: 10,
	}}
	h := New(Dependencies{
		MediaStore:           media,
		ProgressStore:        progress,
		PlaybackSessionStore: absSessions,
		NativeSessions:       nativeSessions,
		NativeSessionSyncer:  &recordingPlaybackSessionSyncer{},
	})

	rec := dispatchABSWithParams(
		http.MethodPatch,
		"/api/session/"+native.ID,
		map[string]string{"sid": native.ID},
		[]byte(`{"currentTime":55.25,"timeListening":10}`),
		"1",
		"profile-1",
		h.handleSessionSync,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if progress.updateCalls != 1 {
		t.Fatalf("updateCalls = %d, want 1", progress.updateCalls)
	}
	if progress.upsertCalls != 0 {
		t.Fatalf("upsertCalls = %d, want 0 (an existing row must not go through a full upsert)", progress.upsertCalls)
	}
}

func TestHandleSessionCloseStopsNativePlaybackSession(t *testing.T) {
	absSessions := &fakePlaybackSessionStore{}
	nativeSessions := playback.NewSessionManager(0, 0)
	native, err := nativeSessions.StartSessionWithFilesContext(context.Background(), 1, "profile-1", 42, 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("start native session: %v", err)
	}
	_ = absSessions.InsertPlaybackSession(context.Background(), ABSPlaybackSession{
		ID:        native.ID,
		UserID:    "1",
		ProfileID: "profile-1",
		ContentID: "book-1",
	})
	syncer := &recordingPlaybackSessionSyncer{}
	h := New(Dependencies{
		MediaStore:           noopMediaStore{},
		PlaybackSessionStore: absSessions,
		NativeSessions:       nativeSessions,
		NativeSessionSyncer:  syncer,
	})

	rec := dispatchABSWithParams(
		http.MethodPost,
		"/api/session/"+native.ID+"/close",
		map[string]string{"sid": native.ID},
		nil,
		"1",
		"profile-1",
		h.handleSessionClose,
	)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := nativeSessions.GetSession(native.ID); err == nil {
		t.Fatalf("native session still exists after close")
	}
	if syncer.calls == 0 {
		t.Fatalf("native session syncer was not called")
	}
}
