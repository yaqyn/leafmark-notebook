package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/yaqyn/leafmark-notebook/internal/database"
)

type leafNote struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Pinned    bool   `json:"pinned"`
	Archived  bool   `json:"archived"`
	Trashed   bool   `json:"trashed"`
	Version   int    `json:"version"`
	WordCount int    `json:"word_count"`
}

const noteColumns = "id,title,note,created_at,updated_at,pinned,archived,trashed,version"

func noteETag(note leafNote) string {
	return strconv.Quote("note-" + strconv.Itoa(note.Version))
}

func scanNote(row interface{ Scan(...any) error }) (n leafNote, err error) {
	err = row.Scan(&n.ID, &n.Title, &n.Note, &n.CreatedAt, &n.UpdatedAt, &n.Pinned, &n.Archived, &n.Trashed, &n.Version)
	n.WordCount = len(strings.Fields(n.Note))
	return
}

func validateNote(w http.ResponseWriter, n *leafNote) bool {
	n.Title = strings.TrimSpace(n.Title)
	if !utf8.ValidString(n.Title) || !utf8.ValidString(n.Note) || strings.IndexFunc(n.Title, unicode.IsControl) >= 0 || utf8.RuneCountInString(n.Title) > 160 || len(n.Note) > 100000 {
		respondWithError(w, 400, "Use a title under 160 characters and a note under 100 KB", nil)
		return false
	}
	for _, char := range n.Note {
		if unicode.IsControl(char) && char != '\n' && char != '\r' && char != '\t' {
			respondWithError(w, 400, "Notes may contain line breaks and tabs, but not other control characters", nil)
			return false
		}
	}
	if n.Title == "" && strings.TrimSpace(n.Note) == "" {
		respondWithError(w, 400, "Write a title or note before saving", nil)
		return false
	}
	return true
}

func (cfg *apiConfig) handlerNotesGet(w http.ResponseWriter, r *http.Request, u database.User) {
	query := r.URL.Query()
	allowed := map[string]bool{"page": true, "page_size": true, "q": true, "title": true, "view": true, "sort": true, "pinned": true, "has_content": true}
	for name := range query {
		if !allowed[name] {
			respondWithError(w, 400, "Unknown note filter", nil)
			return
		}
	}
	for _, name := range []string{"page", "page_size", "q", "title", "view", "sort", "pinned", "has_content"} {
		if len(query[name]) > 1 {
			respondWithError(w, 400, "Duplicate "+name+" parameter", nil)
			return
		}
	}
	pageSize := 30
	if raw := query.Get("page_size"); raw != "" {
		var err error
		pageSize, err = strconv.Atoi(raw)
		if err != nil || pageSize < 1 || pageSize > 100 {
			respondWithError(w, 400, "Page size must be an integer between 1 and 100", nil)
			return
		}
	}
	page := 1
	if raw := query.Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 || page > 100000 {
			respondWithError(w, 400, "Page must be an integer between 1 and 100000", nil)
			return
		}
	}
	q := strings.TrimSpace(query.Get("q"))
	if !utf8.ValidString(q) || utf8.RuneCountInString(q) > 200 {
		respondWithError(w, 400, "Search is too long", nil)
		return
	}
	title := strings.TrimSpace(query.Get("title"))
	if !utf8.ValidString(title) || strings.IndexFunc(title, unicode.IsControl) >= 0 || utf8.RuneCountInString(title) > 160 {
		respondWithError(w, 400, "Title filter is invalid", nil)
		return
	}
	view := query.Get("view")
	filter := "trashed=0 AND archived=0"
	switch view {
	case "archive":
		filter = "trashed=0 AND archived=1"
	case "trash":
		filter = "trashed=1"
	case "pinned":
		filter = "trashed=0 AND archived=0 AND pinned=1"
	case "", "all":
	default:
		respondWithError(w, 400, "Unknown notebook view", nil)
		return
	}
	switch query.Get("pinned") {
	case "":
	case "true":
		filter += " AND pinned=1"
	case "false":
		filter += " AND pinned=0"
	default:
		respondWithError(w, 400, "Pinned must be true or false", nil)
		return
	}
	switch query.Get("has_content") {
	case "":
	case "true":
		filter += " AND trim(note)<>''"
	case "false":
		filter += " AND trim(note)=''"
	default:
		respondWithError(w, 400, "Has content must be true or false", nil)
		return
	}
	order := "updated_at DESC, id"
	switch query.Get("sort") {
	case "", "updated":
	case "created":
		order = "created_at DESC, id"
	case "oldest":
		order = "created_at ASC, id"
	case "title":
		order = "lower(title) ASC, id"
	case "title-desc":
		order = "lower(title) DESC, id"
	default:
		respondWithError(w, 400, "Sort must be updated, created, oldest, title, or title-desc", nil)
		return
	}
	where := ` FROM notes WHERE user_id=? AND ` + filter + ` AND (?='' OR instr(lower(title || char(10) || note),lower(?))>0) AND (?='' OR lower(title)=lower(?))`
	var total int
	if err := cfg.conn.QueryRowContext(r.Context(), "SELECT count(*)"+where, u.ID, q, q, title, title).Scan(&total); err != nil {
		respondWithError(w, 503, "Could not load notes", nil)
		return
	}
	rows, err := cfg.conn.QueryContext(r.Context(), "SELECT "+noteColumns+where+" ORDER BY pinned DESC, "+order+" LIMIT ? OFFSET ?", u.ID, q, q, title, title, pageSize, (page-1)*pageSize)
	if err != nil {
		respondWithError(w, 503, "Could not load notes", nil)
		return
	}
	defer rows.Close()
	notes := []leafNote{}
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			respondWithError(w, 503, "Could not read notes", nil)
			return
		}
		notes = append(notes, n)
	}
	if rows.Err() != nil {
		respondWithError(w, 503, "Could not read notes", nil)
		return
	}
	pages := max(1, (total+pageSize-1)/pageSize)
	w.Header().Set("X-Result-Count", strconv.Itoa(len(notes)))
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	w.Header().Set("X-Has-More", strconv.FormatBool(page < pages))
	w.Header().Set("X-Page", strconv.Itoa(page))
	w.Header().Set("X-Page-Size", strconv.Itoa(pageSize))
	respondWithJSON(w, 200, map[string]any{"notes": notes, "total": total, "page": page, "pages": pages, "page_size": pageSize, "has_previous": page > 1, "has_more": page < pages})
}

func (cfg *apiConfig) handlerNoteGet(w http.ResponseWriter, r *http.Request, u database.User) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		respondWithError(w, 400, "Invalid note identifier", nil)
		return
	}
	note, err := scanNote(cfg.conn.QueryRowContext(r.Context(), "SELECT "+noteColumns+" FROM notes WHERE id=? AND user_id=?", id, u.ID))
	if errors.Is(err, sql.ErrNoRows) {
		respondWithError(w, 404, "Note not found", nil)
		return
	}
	if err != nil {
		respondWithError(w, 503, "Could not load note", nil)
		return
	}
	etag := noteETag(note)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	respondWithJSON(w, 200, note)
}

func (cfg *apiConfig) handlerNotesCreate(w http.ResponseWriter, r *http.Request, u database.User) {
	var n leafNote
	if !decodeJSON(w, r, &n) || !validateNote(w, &n) {
		return
	}
	if n.ID == "" {
		n.ID = uuid.NewString()
	} else if _, err := uuid.Parse(n.ID); err != nil {
		respondWithError(w, 400, "Invalid note identifier", nil)
		return
	}
	n.CreatedAt = timestamp()
	n.UpdatedAt = n.CreatedAt
	n.Version = 1
	n.WordCount = len(strings.Fields(n.Note))
	result, err := cfg.conn.ExecContext(r.Context(), `INSERT INTO notes(id,title,note,created_at,updated_at,user_id,pinned,archived,trashed,version) VALUES(?,?,?,?,?,?,?,0,0,1) ON CONFLICT(id) DO NOTHING`, n.ID, n.Title, n.Note, n.CreatedAt, n.UpdatedAt, u.ID, n.Pinned)
	if err != nil {
		respondWithError(w, 503, "Could not save note; try again", nil)
		return
	}
	count, err := result.RowsAffected()
	if err != nil {
		respondWithError(w, 503, "Could not verify save", nil)
		return
	}
	if count == 0 {
		existing, err := scanNote(cfg.conn.QueryRowContext(r.Context(), "SELECT "+noteColumns+" FROM notes WHERE id=? AND user_id=?", n.ID, u.ID))
		if err == nil && existing.Title == n.Title && existing.Note == n.Note && existing.Pinned == n.Pinned && !existing.Archived && !existing.Trashed {
			w.Header().Set("ETag", noteETag(existing))
			respondWithJSON(w, 200, existing)
			return
		}
		respondWithError(w, 409, "This note was already saved. Download your draft and reload notes before editing again.", nil)
		return
	}
	n.Archived = false
	n.Trashed = false
	w.Header().Set("Location", "/v1/notes/"+n.ID)
	w.Header().Set("ETag", noteETag(n))
	respondWithJSON(w, 201, n)
}

func (cfg *apiConfig) handlerNotesUpdate(w http.ResponseWriter, r *http.Request, u database.User) {
	var n leafNote
	if !decodeJSON(w, r, &n) || !validateNote(w, &n) {
		return
	}
	id := chi.URLParam(r, "id")
	now := timestamp()
	// Ownership and version are checked by the same atomic write.
	saved, err := scanNote(cfg.conn.QueryRowContext(r.Context(), `UPDATE notes SET title=?,note=?,pinned=?,archived=?,trashed=?,updated_at=?,version=version+1 WHERE id=? AND user_id=? AND version=? RETURNING `+noteColumns, n.Title, n.Note, n.Pinned, n.Archived, n.Trashed, now, id, u.ID, n.Version))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		respondWithError(w, 503, "Could not save note; try again", nil)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		var version int
		err = cfg.conn.QueryRowContext(r.Context(), `SELECT version FROM notes WHERE id=? AND user_id=?`, id, u.ID).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			respondWithError(w, 404, "Note not found", nil)
		} else if err != nil {
			respondWithError(w, 503, "Could not verify note", nil)
		} else {
			respondWithError(w, 409, "This note changed in another tab. Export your draft, then reload notes before editing again.", nil)
		}
		return
	}
	w.Header().Set("ETag", noteETag(saved))
	respondWithJSON(w, 200, saved)
}

func (cfg *apiConfig) exportNotes(w http.ResponseWriter, r *http.Request, u database.User) {
	query := r.URL.Query()
	for name := range query {
		if name != "format" && name != "view" {
			respondWithError(w, 400, "Unknown export parameter", nil)
			return
		}
	}
	for _, name := range []string{"format", "view"} {
		if len(query[name]) > 1 {
			respondWithError(w, 400, "Duplicate "+name+" parameter", nil)
			return
		}
	}
	format := query.Get("format")
	if format != "" && format != "json" && format != "markdown" {
		respondWithError(w, 400, "Export format must be json or markdown", nil)
		return
	}
	filter := ""
	switch query.Get("view") {
	case "", "all":
	case "active":
		filter = " AND trashed=0 AND archived=0"
	case "archive":
		filter = " AND trashed=0 AND archived=1"
	case "trash":
		filter = " AND trashed=1"
	default:
		respondWithError(w, 400, "Export view must be all, active, archive, or trash", nil)
		return
	}
	rows, err := cfg.conn.QueryContext(r.Context(), "SELECT "+noteColumns+" FROM notes WHERE user_id=?"+filter+" ORDER BY created_at,id", u.ID)
	if err != nil {
		respondWithError(w, 503, "Could not export notes", nil)
		return
	}
	defer rows.Close()
	notes := []leafNote{}
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			respondWithError(w, 503, "Could not read export", nil)
			return
		}
		notes = append(notes, n)
	}
	if rows.Err() != nil {
		respondWithError(w, 503, "Could not finish export", nil)
		return
	}
	if format == "markdown" {
		var document strings.Builder
		document.WriteString("# Leafmark notes\n")
		for _, note := range notes {
			title := note.Title
			if title == "" {
				title = "Untitled note"
			}
			document.WriteString("\n## " + title + "\n")
			states := []string{}
			if note.Pinned {
				states = append(states, "pinned")
			}
			if note.Archived {
				states = append(states, "archived")
			}
			if note.Trashed {
				states = append(states, "trashed")
			}
			if len(states) > 0 {
				document.WriteString("\n> Status: " + strings.Join(states, ", ") + "\n")
			}
			document.WriteString("\n" + note.Note + "\n")
		}
		w.Header().Set("Content-Disposition", `attachment; filename="leafmark-notes.md"`)
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(document.String()))
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="leafmark-notes.json"`)
	respondWithJSON(w, 200, map[string]any{"format": "leafmark-v1", "exported_at": timestamp(), "notes": notes})
}
