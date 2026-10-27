package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// previewLen is how many characters of a paste the recent list shows.
const previewLen = 80

var errNotFound = errors.New("paste not found")

type paste struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type Store interface {
	Create(ctx context.Context, id, content string) error
	Get(ctx context.Context, id string) (paste, error)
	// Recent returns the newest pastes first, with their full Content.
	Recent(ctx context.Context, n int) ([]paste, error)
	Close()
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// preview cuts s to previewLen characters without converting the whole
// string to runes (pastes can be 1 MiB).
func preview(s string) string {
	n := 0
	for i := range s {
		if n == previewLen {
			return s[:i]
		}
		n++
	}
	return s
}

// postgres

const schema = `
CREATE TABLE IF NOT EXISTS pastes (
	id         TEXT PRIMARY KEY,
	content    TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

type pgStore struct {
	db *pgxpool.Pool
}

// openPG fails if Postgres can't be reached; pgxpool.New alone connects lazily.
func openPG(ctx context.Context, dsn string) (*pgStore, error) {
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(ctx, schema); err != nil {
		db.Close()
		return nil, err
	}
	return &pgStore{db: db}, nil
}

func (s *pgStore) Create(ctx context.Context, id, content string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO pastes (id, content) VALUES ($1, $2)`, id, content)
	return err
}

func (s *pgStore) Get(ctx context.Context, id string) (paste, error) {
	var p paste
	err := s.db.QueryRow(ctx,
		`SELECT id, content, created_at FROM pastes WHERE id = $1`, id).
		Scan(&p.ID, &p.Content, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, errNotFound
	}
	return p, err
}

func (s *pgStore) Recent(ctx context.Context, n int) ([]paste, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, content, created_at FROM pastes ORDER BY created_at DESC LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[paste])
}

func (s *pgStore) Close() { s.db.Close() }

// file: one JSON object per line, appended on every paste and held in
// memory for reads.

type fileStore struct {
	mu     sync.RWMutex
	f      *os.File
	pastes []paste // oldest first
	byID   map[string]int
}

// openFile loads the paste file. readOnly never creates or writes to it, so
// the CLI can read it while a server is appending.
func openFile(path string, readOnly bool) (*fileStore, error) {
	flags := os.O_RDWR | os.O_CREATE | os.O_APPEND
	if readOnly {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	s := &fileStore{f: f, byID: map[string]int{}}

	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var p paste
			if jerr := json.Unmarshal(line, &p); jerr != nil {
				slog.Warn("skipping bad line", "path", path, "err", jerr)
			} else {
				s.byID[p.ID] = len(s.pastes)
				s.pastes = append(s.pastes, p)
			}
		}
		if err == io.EOF {
			// a crash mid-write leaves no trailing newline; add one so the
			// next append doesn't glue onto the broken line
			if len(line) > 0 && !readOnly {
				f.WriteString("\n")
			}
			break
		}
		if err != nil {
			f.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *fileStore) Create(_ context.Context, id, content string) error {
	p := paste{ID: id, Content: content, CreatedAt: time.Now()}
	line, err := json.Marshal(p)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Write(append(line, '\n')); err != nil {
		return err
	}
	s.byID[p.ID] = len(s.pastes)
	s.pastes = append(s.pastes, p)
	return nil
}

func (s *fileStore) Get(_ context.Context, id string) (paste, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i, ok := s.byID[id]
	if !ok {
		return paste{}, errNotFound
	}
	return s.pastes[i], nil
}

func (s *fileStore) Recent(_ context.Context, n int) ([]paste, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []paste
	for i := len(s.pastes) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, s.pastes[i])
	}
	return out, nil
}

func (s *fileStore) Close() { s.f.Close() }
