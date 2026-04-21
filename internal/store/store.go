package store

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type FileRecord struct {
	Key       string
	Path      string
	Name      string
	Size      int64
	SHA256    string
	UpdatedAt time.Time
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	stmts := []string{
		`create table if not exists peers (
			peer_id text primary key,
			addrs text not null,
			last_seen text not null,
			last_connected text not null default ''
		)`,
		`create table if not exists files (
			key text primary key,
			path text not null,
			name text not null,
			size integer not null,
			sha256 text not null,
			updated_at text not null
		)`,
		`create table if not exists services (
			name text primary key,
			command text not null,
			advertised_at text not null
		)`,
		`create table if not exists transfers (
			id integer primary key autoincrement,
			kind text not null,
			key text not null,
			peer_id text not null,
			status text not null,
			bytes integer not null,
			started_at text not null,
			ended_at text not null
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) UpsertPeer(peerID, addrs string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		insert into peers(peer_id, addrs, last_seen)
		values(?, ?, ?)
		on conflict(peer_id) do update set addrs=excluded.addrs, last_seen=excluded.last_seen
	`, peerID, addrs, now)
	return err
}

func (s *Store) UpsertFile(rec FileRecord) error {
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(`
		insert into files(key, path, name, size, sha256, updated_at)
		values(?, ?, ?, ?, ?, ?)
		on conflict(key) do update set
			path=excluded.path,
			name=excluded.name,
			size=excluded.size,
			sha256=excluded.sha256,
			updated_at=excluded.updated_at
	`, rec.Key, rec.Path, rec.Name, rec.Size, rec.SHA256, rec.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) RecordService(name, command string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		insert into services(name, command, advertised_at)
		values(?, ?, ?)
		on conflict(name) do update set command=excluded.command, advertised_at=excluded.advertised_at
	`, name, command, now)
	return err
}

func (s *Store) RecordTransfer(kind, key, peerID, status string, bytes int64, started, ended time.Time) error {
	_, err := s.db.Exec(`
		insert into transfers(kind, key, peer_id, status, bytes, started_at, ended_at)
		values(?, ?, ?, ?, ?, ?, ?)
	`, kind, key, peerID, status, bytes, started.UTC().Format(time.RFC3339Nano), ended.UTC().Format(time.RFC3339Nano))
	return err
}
