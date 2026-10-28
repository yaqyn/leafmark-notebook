package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yaqyn/leafmark-notebook/internal/database"
)

func testApp(t *testing.T) (*apiConfig, http.Handler) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err = migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	cfg := &apiConfig{DB: database.New(db), conn: db, allowSignup: true, limiter: newRateLimiter()}
	return cfg, cfg.routes()
}
func request(h http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func signup(t *testing.T, h http.Handler, name string) *http.Cookie {
	t.Helper()
	w := request(h, "POST", "/v1/auth/register", `{"name":"Test","username":"`+name+`","password":"correct horse battery staple"}`, nil)
	if w.Code != 201 {
		t.Fatalf("signup: %d %s", w.Code, w.Body)
	}
	return w.Result().Cookies()[0]
}
func TestOwnershipConflictAndTrash(t *testing.T) {
	_, h := testApp(t)
	alice := signup(t, h, "alice")
	bob := signup(t, h, "bob")
	w := request(h, "POST", "/v1/notes", `{"title":"Private","note":"original"}`, alice)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var n leafNote
	if err := json.Unmarshal(w.Body.Bytes(), &n); err != nil {
		t.Fatal(err)
	}
	w = request(h, "PUT", "/v1/notes/"+n.ID, `{"title":"Stolen","version":1}`, bob)
	if w.Code != 404 {
		t.Fatal("cross-owner write", w.Code)
	}
	w = request(h, "PUT", "/v1/notes/"+n.ID, `{"title":"Saved","note":"edited","version":1}`, alice)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var saved leafNote
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.CreatedAt != n.CreatedAt || saved.Version != 2 {
		t.Fatal("update response lost metadata", saved)
	}
	w = request(h, "PUT", "/v1/notes/"+n.ID, `{"title":"Stale","version":1}`, alice)
	if w.Code != 409 {
		t.Fatal("stale write", w.Code)
	}
	w = request(h, "PUT", "/v1/notes/"+n.ID, `{"title":"Saved","note":"edited","trashed":true,"version":2}`, alice)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = request(h, "GET", "/v1/notes", ``, alice)
	if !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatal(w.Body)
	}
	w = request(h, "GET", "/v1/notes?view=trash", ``, alice)
	if !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatal(w.Body)
	}
	w = request(h, "PUT", "/v1/notes/"+n.ID, `{"title":"Saved","note":"edited","trashed":false,"version":3}`, alice)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = request(h, "GET", "/v1/notes?q=edited", ``, alice)
	if !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatal(w.Body)
	}
	w = request(h, "GET", "/v1/export", ``, bob)
	if strings.Contains(w.Body.String(), "edited") {
		t.Fatal("export leaked another user's note")
	}
}

func TestInvalidNotePagesAreNotSilentlyReplacedWithPageOne(t *testing.T) {
	_, h := testApp(t)
	cookie := signup(t, h, "pager")
	for _, page := range []string{"bad", "0", "-1", "100001", "999999999999999999999999"} {
		response := request(h, "GET", "/v1/notes?page="+page, "", cookie)
		if response.Code != 400 {
			t.Fatalf("page %q: %d %s", page, response.Code, response.Body)
		}
	}
	for _, query := range []string{"", "?page=1", "?page=100000"} {
		response := request(h, "GET", "/v1/notes"+query, "", cookie)
		if response.Code != 200 {
			t.Fatalf("%q: %d %s", query, response.Code, response.Body)
		}
	}
}
func TestNoteSearchLimitCountsCharactersNotUTF8Bytes(t *testing.T) {
	_, h := testApp(t)
	cookie := signup(t, h, "unicode-search")
	for query, want := range map[string]int{
		strings.Repeat("ش", 200): 200,
		strings.Repeat("ش", 201): 400,
	} {
		response := request(h, "GET", "/v1/notes?q="+query, "", cookie)
		if response.Code != want {
			t.Fatalf("%d characters: %d %s", len([]rune(query)), response.Code, response.Body)
		}
	}
}
func TestDuplicateNoteFiltersAreRejected(t *testing.T) {
	_, h := testApp(t)
	cookie := signup(t, h, "duplicate-filter")
	for _, query := range []string{"?page=1&page=2", "?page_size=10&page_size=20", "?q=one&q=two", "?view=all&view=trash", "?sort=created&sort=updated"} {
		response := request(h, "GET", "/v1/notes"+query, "", cookie)
		if response.Code != 400 {
			t.Fatalf("%s: %d %s", query, response.Code, response.Body)
		}
	}
}
func TestNoteSortModesAreValidated(t *testing.T) {
	cfg, h := testApp(t)
	cookie := signup(t, h, "sort-mode")
	for _, title := range []string{"Older", "Newer"} {
		if response := request(h, "POST", "/v1/notes", `{"title":"`+title+`"}`, cookie); response.Code != 201 {
			t.Fatal(response.Code, response.Body)
		}
	}
	if _, err := cfg.conn.Exec(`UPDATE notes SET created_at='2025-01-01T00:00:00Z' WHERE title='Older'`); err != nil {
		t.Fatal(err)
	}
	for _, sort := range []string{"created", "updated", "oldest"} {
		if response := request(h, "GET", "/v1/notes?sort="+sort, "", cookie); response.Code != 200 {
			t.Fatalf("%s: %d %s", sort, response.Code, response.Body)
		}
	}
	response := request(h, "GET", "/v1/notes?sort=oldest", "", cookie)
	var page struct {
		Notes []leafNote `json:"notes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || len(page.Notes) != 2 || page.Notes[0].Title != "Older" {
		t.Fatalf("oldest ordering: %d %s", response.Code, response.Body.String())
	}
	if response := request(h, "GET", "/v1/notes?sort=random", "", cookie); response.Code != 400 {
		t.Fatalf("invalid sort: %d %s", response.Code, response.Body)
	}
}
func TestNotePageSizeIsBoundedAndReported(t *testing.T) {
	_, h := testApp(t)
	cookie := signup(t, h, "page-size")
	for _, title := range []string{"One", "Two"} {
		if response := request(h, "POST", "/v1/notes", `{"title":"`+title+`"}`, cookie); response.Code != 201 {
			t.Fatal(response.Code, response.Body)
		}
	}
	response := request(h, "GET", "/v1/notes?page_size=1", "", cookie)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"page_size":1`) || !strings.Contains(response.Body.String(), `"total":2`) || !strings.Contains(response.Body.String(), `"pages":2`) || !strings.Contains(response.Body.String(), `"has_more":true`) {
		t.Fatal(response.Code, response.Body)
	}
	response = request(h, "GET", "/v1/notes?page_size=1&page=2", "", cookie)
	if !strings.Contains(response.Body.String(), `"has_previous":true`) || !strings.Contains(response.Body.String(), `"has_more":false`) {
		t.Fatal(response.Code, response.Body)
	}
	for _, size := range []string{"0", "101", "bad"} {
		if response := request(h, "GET", "/v1/notes?page_size="+size, "", cookie); response.Code != 400 {
			t.Fatalf("%s: %d %s", size, response.Code, response.Body)
		}
	}
}
func TestUnknownNoteFiltersAreRejected(t *testing.T) {
	_, h := testApp(t)
	cookie := signup(t, h, "unknown-filter")
	response := request(h, "GET", "/v1/notes?veiw=trash", "", cookie)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", response.Code, response.Body)
	}
}
func TestWhitespaceOnlyNoteSearchShowsAllNotes(t *testing.T) {
	_, h := testApp(t)
	cookie := signup(t, h, "space-search")
	if response := request(h, "POST", "/v1/notes", `{"title":"Solo"}`, cookie); response.Code != 201 {
		t.Fatal(response.Code, response.Body)
	}
	response := request(h, "GET", "/v1/notes?q=%20%20", "", cookie)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"total":1`) {
		t.Fatal(response.Code, response.Body)
	}
}
func TestExactTitleFilter(t *testing.T) {
	_, h := testApp(t)
	cookie := signup(t, h, "title-filter")
	for _, title := range []string{"Plan", "Planning"} {
		if response := request(h, "POST", "/v1/notes", `{"title":"`+title+`"}`, cookie); response.Code != 201 {
			t.Fatal(response.Code, response.Body)
		}
	}
	response := request(h, "GET", "/v1/notes?title=PLAN", "", cookie)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"title":"Plan"`) || strings.Contains(response.Body.String(), `"title":"Planning"`) {
		t.Fatal(response.Code, response.Body)
	}
}
func TestSessionsValidationAndCSRF(t *testing.T) {
	cfg, h := testApp(t)
	cookie := signup(t, h, "tester")
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe session cookie")
	}
	for _, body := range []string{`{`, `{"title":""}`, `{"title":"line\nbreak"}`, `{"title":"ok","unknown":1}`, `{"title":"ok"} {`} {
		if w := request(h, "POST", "/v1/notes", body, cookie); w.Code != 400 {
			t.Fatal(body, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/v1/notes", strings.NewReader(`{"title":"attack"}`))
	r.Header.Set("Origin", "https://attacker.example")
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("CSRF accepted", w.Code)
	}
	w = request(h, "POST", "/v1/auth/login", `{"username":"tester","password":"wrong"}`, nil)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	w = request(h, "POST", "/v1/auth/login", `{"username":"tester","password":"correct horse battery staple"}`, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = request(h, "POST", "/v1/auth/logout", ``, cookie)
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	w = request(h, "GET", "/v1/users", ``, cookie)
	if w.Code != 401 {
		t.Fatal("revoked session accepted", w.Code)
	}
	cfg.allowSignup = false
	w = request(h, "POST", "/v1/auth/register", `{}`, nil)
	if w.Code != 403 {
		t.Fatal("registration switch ignored")
	}
}
func TestMigrationPreservesCourseData(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, name := range []string{"001_users.sql", "002_notes.sql"} {
		data, _ := schemaFiles.ReadFile("sql/schema/" + name)
		up := strings.Split(strings.TrimPrefix(string(data), "-- +goose Up"), "-- +goose Down")[0]
		if _, err = db.Exec(up); err != nil {
			t.Fatal(err)
		}
	}
	_, err = db.Exec(`INSERT INTO users VALUES('old','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','Old user','legacy-secret'); INSERT INTO notes VALUES('old-note','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','Keep me','old')`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = migrate(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &apiConfig{DB: database.New(db), conn: db, allowSignup: true, limiter: newRateLimiter()}
	h := cfg.routes()
	w := request(h, "POST", "/v1/auth/claim", `{"username":"old-user","password":"correct horse battery staple","legacy_key":"legacy-secret"}`, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = request(h, "GET", "/v1/notes", ``, w.Result().Cookies()[0])
	if !strings.Contains(w.Body.String(), "Keep me") {
		t.Fatal("legacy data lost", w.Body)
	}
	w = request(h, "POST", "/v1/auth/claim", `{"username":"other-user","password":"correct horse battery staple","legacy_key":"legacy-secret"}`, nil)
	if w.Code != 401 {
		t.Fatal("legacy key still usable", w.Code)
	}
}
func TestAuthRateLimit(t *testing.T) {
	_, h := testApp(t)
	for range 20 {
		request(h, "POST", "/v1/auth/login", `{}`, nil)
	}
	w := request(h, "POST", "/v1/auth/login", `{}`, nil)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal(w.Code)
	}
}

func TestCreateRetryIsIdempotent(t *testing.T) {
	_, h := testApp(t)
	cookie := signup(t, h, "retry-user")
	body := `{"id":"01234567-89ab-4cde-8fab-0123456789ab","title":"Retry safe","note":"One copy"}`
	for i := range 2 {
		w := request(h, "POST", "/v1/notes", body, cookie)
		if w.Code != 201 && !(i == 1 && w.Code == 200) {
			t.Fatal(w.Code, w.Body)
		}
	}
	w := request(h, "GET", "/v1/notes", "", cookie)
	if !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatal(w.Body)
	}
	w = request(h, "POST", "/v1/notes", strings.Replace(body, "One copy", "Changed after uncertainty", 1), cookie)
	if w.Code != 409 {
		t.Fatal("retry overwrote stored note", w.Code)
	}
}

func TestSessionStorageExpiryAndReadinessFailure(t *testing.T) {
	cfg, h := testApp(t)
	cfg.secureCookies = true
	cookie := signup(t, h, "expiry-user")
	if !cookie.Secure {
		t.Fatal("production cookie is not Secure")
	}
	var stored string
	if err := cfg.conn.QueryRow(`SELECT token_hash FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == cookie.Value || stored != hashToken(cookie.Value) {
		t.Fatal("session token not hashed")
	}
	if _, err := cfg.conn.Exec(`UPDATE sessions SET expires_at='2000-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	w := request(h, "GET", "/v1/users", "", cookie)
	if w.Code != 401 {
		t.Fatal("expired session accepted", w.Code)
	}
	cfg.conn.Close()
	w = request(h, "GET", "/v1/readyz", "", nil)
	if w.Code != 503 {
		t.Fatal("readiness ignored database failure", w.Code)
	}
}
