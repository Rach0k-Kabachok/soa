package main

import (
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/shopspring/decimal"
	"marketplace/catalog-service/internal/marketplace"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	decimal.MarshalJSONWithoutQuotes = true
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		fatal(err)
	}
	minutes := 1
	if value := os.Getenv("ORDER_INTERVAL_MINUTES"); value != "" {
		minutes, err = strconv.Atoi(value)
		if err != nil || minutes < 0 {
			slog.Error("ORDER_INTERVAL_MINUTES must be non-negative")
			os.Exit(1)
		}
	}
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		slog.Error("JWT_SECRET is required")
		os.Exit(1)
	}
	handler, err := marketplace.NewHandler(db, secret, time.Duration(minutes)*time.Minute)
	if err != nil {
		fatal(err)
	}

	server := &http.Server{
		Addr:              ":8080",
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Info("catalog-service listening on :8080")
	fatal(server.ListenAndServe())
}

func fatal(err error) {
	slog.Error("startup failed", "error", err.Error())
	os.Exit(1)
}
