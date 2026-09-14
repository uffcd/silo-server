package handlers

import (
	"errors"
	"testing"
)

func TestAdminSettingsCheckServiceValidationAndSafeFailure(t *testing.T) {
	h := &AdminHandler{SettingsRepo: &fakeServerSettingsStore{values: map[string]string{}}}
	if _, err := h.CheckAdminSettingsConnection(t.Context(), "unknown", nil, nil); !errors.Is(err, ErrAdminSettingsCheckKind) {
		t.Fatal(err)
	}
	// Missing credentials fail locally without a real provider call.
	result, err := h.CheckAdminSettingsConnection(t.Context(), "mdblist", nil, nil)
	if err != nil || result.Success || result.Message != "Connection check failed. Verify the submitted settings and provider availability." {
		t.Fatal(result, err)
	}
	if _, err := (&AdminHandler{}).CheckAdminSettingsConnection(t.Context(), "redis", nil, nil); !errors.Is(err, ErrAdminSettingsUnavailable) {
		t.Fatal(err)
	}
}
