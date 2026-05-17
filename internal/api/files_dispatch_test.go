package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFilesDispatcher_Get(t *testing.T) {
	var called string
	stub := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			called = name
			w.WriteHeader(http.StatusOK)
		}
	}

	d := NewFilesDispatcher(
		stub("list"), stub("stat"), stub("download"),
		stub("mkdir"), stub("upload"), stub("uploadFolder"),
		stub("downloadFolder"), stub("rename"), stub("delete"),
	)

	tests := []struct {
		op      string
		want    string
		wantErr bool
	}{
		{"list", "list", false},
		{"stat", "stat", false},
		{"download-folder", "downloadFolder", false},
		{"", "download", false}, // Phase 3 backward compat
		{"bogus", "", true},
	}

	for _, tc := range tests {
		t.Run("op="+tc.op, func(t *testing.T) {
			called = ""
			req := httptest.NewRequest(http.MethodGet, "/files?op="+tc.op, nil)
			if tc.op == "" {
				req = httptest.NewRequest(http.MethodGet, "/files?path=/sdcard/x", nil)
			}
			w := httptest.NewRecorder()
			d.Get(w, req)

			if tc.wantErr {
				if w.Code != http.StatusBadRequest {
					t.Fatalf("op=%q: expected 400, got %d", tc.op, w.Code)
				}
				var body map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("op=%q: failed to parse error body: %v", tc.op, err)
				}
				errObj, _ := body["error"].(map[string]any)
				if errObj["code"] != "UNSUPPORTED_OP" {
					t.Errorf("op=%q: expected UNSUPPORTED_OP, got %v", tc.op, errObj["code"])
				}
				return
			}
			if called != tc.want {
				t.Errorf("op=%q: called %q, want %q", tc.op, called, tc.want)
			}
		})
	}
}

func TestFilesDispatcher_Post(t *testing.T) {
	var called string
	stub := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			called = name
			w.WriteHeader(http.StatusOK)
		}
	}

	d := NewFilesDispatcher(
		stub("list"), stub("stat"), stub("download"),
		stub("mkdir"), stub("upload"), stub("uploadFolder"),
		stub("downloadFolder"), stub("rename"), stub("delete"),
	)

	tests := []struct {
		op      string
		want    string
		wantErr bool
	}{
		{"mkdir", "mkdir", false},
		{"upload-folder", "uploadFolder", false},
		{"", "upload", false}, // Phase 3 backward compat
		{"bogus", "", true},
	}

	for _, tc := range tests {
		t.Run("op="+tc.op, func(t *testing.T) {
			called = ""
			req := httptest.NewRequest(http.MethodPost, "/files?op="+tc.op, nil)
			if tc.op == "" {
				req = httptest.NewRequest(http.MethodPost, "/files?path=/sdcard/x", nil)
			}
			w := httptest.NewRecorder()
			d.Post(w, req)

			if tc.wantErr {
				if w.Code != http.StatusBadRequest {
					t.Fatalf("op=%q: expected 400, got %d", tc.op, w.Code)
				}
				return
			}
			if called != tc.want {
				t.Errorf("op=%q: called %q, want %q", tc.op, called, tc.want)
			}
		})
	}
}

func TestFilesDispatcher_Patch(t *testing.T) {
	var called string
	stub := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			called = name
			w.WriteHeader(http.StatusOK)
		}
	}

	d := NewFilesDispatcher(
		stub("list"), stub("stat"), stub("download"),
		stub("mkdir"), stub("upload"), stub("uploadFolder"),
		stub("downloadFolder"), stub("rename"), stub("delete"),
	)

	t.Run("op=rename", func(t *testing.T) {
		called = ""
		req := httptest.NewRequest(http.MethodPatch, "/files?op=rename&path=/sdcard/a&to=/sdcard/b", nil)
		w := httptest.NewRecorder()
		d.Patch(w, req)
		if called != "rename" {
			t.Errorf("called %q, want %q", called, "rename")
		}
	})

	t.Run("no_op", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/files", nil)
		w := httptest.NewRecorder()
		d.Patch(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("failed to parse error body: %v", err)
		}
		errObj, _ := body["error"].(map[string]any)
		if errObj["code"] != "UNSUPPORTED_OP" {
			t.Errorf("expected UNSUPPORTED_OP, got %v", errObj["code"])
		}
	})
}

func TestFilesDispatcher_Delete(t *testing.T) {
	var called string
	stub := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			called = name
			w.WriteHeader(http.StatusOK)
		}
	}

	d := NewFilesDispatcher(
		stub("list"), stub("stat"), stub("download"),
		stub("mkdir"), stub("upload"), stub("uploadFolder"),
		stub("downloadFolder"), stub("rename"), stub("delete"),
	)

	t.Run("no_op", func(t *testing.T) {
		called = ""
		req := httptest.NewRequest(http.MethodDelete, "/files?path=/sdcard/x", nil)
		w := httptest.NewRecorder()
		d.Delete(w, req)
		if called != "delete" {
			t.Errorf("called %q, want %q", called, "delete")
		}
	})

	t.Run("recursive=1", func(t *testing.T) {
		called = ""
		req := httptest.NewRequest(http.MethodDelete, "/files?path=/sdcard/x&recursive=1", nil)
		w := httptest.NewRecorder()
		d.Delete(w, req)
		if called != "delete" {
			t.Errorf("called %q, want %q (recursive handled inside DeleteFile)", called, "delete")
		}
	})

	t.Run("op=nuke", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/files?path=/sdcard/x&op=nuke", nil)
		w := httptest.NewRecorder()
		d.Delete(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for unknown op on DELETE, got %d", w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("failed to parse error body: %v", err)
		}
		errObj, _ := body["error"].(map[string]any)
		if errObj["code"] != "UNSUPPORTED_OP" {
			t.Errorf("expected UNSUPPORTED_OP, got %v", errObj["code"])
		}
	})
}