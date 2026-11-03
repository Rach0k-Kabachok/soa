package storage

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*/*.sql
var migrations embed.FS

func Open(ctx context.Context, service string) (*pgxpool.Pool, error) {
	db, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return nil, err
	}
	if err = db.Ping(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err = migrate(ctx, db, service); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(ctx context.Context, db *pgxpool.Pool, service string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1); CREATE TABLE IF NOT EXISTS schema_migrations(version text PRIMARY KEY)`); err != nil {
		return err
	}
	files, err := migrations.ReadDir("migrations/" + service)
	if err != nil {
		return err
	}
	for _, file := range files {
		var exists bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)", file.Name()).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sql, err := migrations.ReadFile("migrations/" + service + "/" + file.Name())
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %s: %w", file.Name(), err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations VALUES($1)", file.Name()); err != nil {
			return err
		}
		slog.Info("migration applied", "service", service, "version", file.Name())
	}
	return tx.Commit(ctx)
}

func Env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func Duration(key, fallback string) time.Duration {
	d, err := time.ParseDuration(Env(key, fallback))
	if err != nil || d <= 0 {
		panic("invalid positive duration: " + key)
	}
	return d
}

func Lock(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", id)
	return err
}

// A session lock spans the durable intent and the following local transaction
// on one connection. Holding a transaction and borrowing a second connection
// for the intent could exhaust the pool under concurrent booking requests.
func AcquireLock(ctx context.Context, db *pgxpool.Pool, id string) (*pgxpool.Conn, func(), error) {
	conn, err := db.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1,0))", id); err != nil {
		conn.Conn().Close(context.Background())
		conn.Release()
		return nil, nil, err
	}
	unlock := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", id); err != nil {
			conn.Conn().Close(context.Background())
		}
		conn.Release()
	}
	return conn, unlock, nil
}
