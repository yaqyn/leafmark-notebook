package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/joho/godotenv"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
	"github.com/yaqyn/leafmark-notebook/internal/database"
	_ "modernc.org/sqlite"
)

type apiConfig struct {
	DB            *database.Queries
	conn          *sql.DB
	secureCookies bool
	allowSignup   bool
	limiter       *rateLimiter
}

//go:embed static/*
var staticFiles embed.FS

//go:embed sql/schema/*.sql
var schemaFiles embed.FS

func main() {
	_ = godotenv.Load()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	dbURL := os.Getenv("DATABASE_URL")
	driver := "libsql"
	if dbURL == "" {
		dbURL = "file:leafmark.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
		driver = "sqlite"
	}
	db, err := sql.Open(driver, dbURL)
	if err != nil {
		slog.Error("database open failed")
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = db.PingContext(ctx)
	if err == nil {
		err = migrate(ctx, db)
	}
	cancel()
	if err != nil {
		slog.Error("database initialization failed; check connection and schema")
		os.Exit(1)
	}
	cfg := &apiConfig{DB: database.New(db), conn: db, secureCookies: os.Getenv("COOKIE_SECURE") != "false", allowSignup: os.Getenv("ALLOW_SIGNUP") != "false", limiter: newRateLimiter()}
	srv := &http.Server{Addr: ":" + port, Handler: cfg.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	shutdownDone := make(chan struct{})
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		defer close(shutdownDone)
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("graceful shutdown failed")
		}
	}()
	slog.Info("Leafmark listening", "port", port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("HTTP server failed")
		os.Exit(1)
	}
	<-shutdownDone
}

func (cfg *apiConfig) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
			w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			if cfg.secureCookies {
				w.Header().Set("Strict-Transport-Security", "max-age=31536000")
			}
			if len(r.URL.Path) >= 4 && r.URL.Path[:4] == "/v1/" {
				w.Header().Set("Cache-Control", "no-store")
			}
			next.ServeHTTP(w, r)
		})
	})
	r.Get("/v1/healthz", handlerReadiness)
	r.Get("/v1/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if cfg.conn.PingContext(ctx) != nil {
			respondWithError(w, 503, "Database unavailable", nil)
			return
		}
		respondWithJSON(w, 200, map[string]string{"status": "ready"})
	})
	r.Post("/v1/auth/register", cfg.limitAuth(cfg.register))
	r.Post("/v1/auth/login", cfg.limitAuth(cfg.login))
	r.Post("/v1/auth/claim", cfg.limitAuth(cfg.claimLegacy))
	r.Post("/v1/auth/logout", cfg.logout)
	r.Get("/v1/users", cfg.middlewareAuth(cfg.handlerUsersGet))
	r.Get("/v1/notes", cfg.middlewareAuth(cfg.handlerNotesGet))
	r.Get("/v1/notes/{id}", cfg.middlewareAuth(cfg.handlerNoteGet))
	r.Post("/v1/notes", cfg.middlewareAuth(cfg.handlerNotesCreate))
	r.Put("/v1/notes/{id}", cfg.middlewareAuth(cfg.handlerNotesUpdate))
	r.Get("/v1/export", cfg.middlewareAuth(cfg.exportNotes))
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { respondWithError(w, 404, "Page not found", nil) })
	assets, _ := fs.Sub(staticFiles, "static")
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/index.html"
		data, _ := staticFiles.ReadFile("static/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})
	r.Handle("/assets/*", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
	return http.NewCrossOriginProtection().Handler(r)
}
