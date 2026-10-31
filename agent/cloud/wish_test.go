package cloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"briard.io/shared/api"
)

// The wish is two form fields at WishPath and nothing else: no token, no node name, no JSON.
func TestSendWishPostsTheTwoFields(t *testing.T) {
	var got http.Header
	var form map[string][]string
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, got = r.URL.Path, r.Header.Clone()
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		form = r.PostForm
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := SendWish(context.Background(), srv.URL+"/", api.Wish{App: "paperless-ngx", Voter: "ab12"}); err != nil {
		t.Fatal(err)
	}
	if path != api.WishPath {
		t.Errorf("path = %q, want %q", path, api.WishPath)
	}
	if got.Get("Authorization") != "" {
		t.Errorf("a wish carries no token, got %q", got.Get("Authorization"))
	}
	if len(form) != 2 || form["app"][0] != "paperless-ngx" || form["voter"][0] != "ab12" {
		t.Errorf("form = %v, want app and voter only", form)
	}
}

// A refusal is an error carrying the server's line, so the caller's retry and its log see it.
func TestSendWishRefusalIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not a catalog name", http.StatusBadRequest)
	}))
	defer srv.Close()
	err := SendWish(context.Background(), srv.URL, api.Wish{App: "x"})
	if err == nil || !strings.Contains(err.Error(), "not a catalog name") {
		t.Fatalf("err = %v, want the server's refusal", err)
	}
}
