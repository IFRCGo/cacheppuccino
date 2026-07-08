package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/sqliteshim"
)

const (
	metaKeyLastPull       = "last_pull_rfc3339"
	metaKeyLastHash       = "last_xlsx_sha256"
	metaKeyLastPullError  = "last_pull_error"
	metaKeyLastImportRows = "last_import_rows"
)

type DB struct {
	sql *sql.DB
	bun *bun.DB
}

func OpenDB(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	sqldb, err := sql.Open(sqliteshim.ShimName, "file:"+path+"?mode=rwc")
	if err != nil {
		return nil, err
	}

	// Single connection: pragmas below stick for the process lifetime,
	// and SQLite sees one writer, so no busy contention between our own queries.
	sqldb.SetMaxOpenConns(1)
	sqldb.SetConnMaxLifetime(0)

	// Set via Exec (not DSN params) so behavior is identical across the
	// cgo (mattn) and pure-Go (modernc) drivers sqliteshim may pick.
	pragmas := []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA temp_store = MEMORY",
	}
	for _, p := range pragmas {
		if _, err := sqldb.Exec(p); err != nil {
			_ = sqldb.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}

	bdb := bun.NewDB(sqldb, sqlitedialect.New())

	db := &DB{sql: sqldb, bun: bdb}
	if err := db.migrate(context.Background()); err != nil {
		_ = sqldb.Close()
		return nil, err
	}

	return db, nil
}

func (db *DB) Close() error {
	return db.sql.Close()
}

func (db *DB) migrate(ctx context.Context) error {
	_, err := db.bun.NewCreateTable().
		Model((*StringModel)(nil)).
		IfNotExists().
		Exec(ctx)
	if err != nil {
		return err
	}

	_, err = db.bun.NewCreateTable().
		Model((*MetaModel)(nil)).
		IfNotExists().
		Exec(ctx)
	if err != nil {
		return err
	}

	_, err = db.bun.NewCreateIndex().
		Model((*StringModel)(nil)).
		Index("idx_strings_page_lang").
		IfNotExists().
		Column("page", "lang").
		Exec(ctx)

	return err
}

// HasStrings reports whether the cache holds any servable data.
func (db *DB) HasStrings(ctx context.Context) (bool, error) {
	return db.bun.NewSelect().
		Model((*StringModel)(nil)).
		Exists(ctx)
}

// ReplaceImport atomically replaces the entire strings table with the given
// rows and records the import metadata. The XLSX is the complete source of
// truth, so rows absent from it must not survive an import.
func (db *DB) ReplaceImport(ctx context.Context, rows []StringRow, hash string, pulledAt time.Time) error {
	// SQLite default max variables is often 999.
	// We insert 5 columns per row: page, key, lang, value, updated_at.
	const (
		sqliteMaxVars  = 999
		colsPerRow     = 5
		safetyHeadroom = 50
	)
	const maxRowsPerStmt = (sqliteMaxVars - safetyHeadroom) / colsPerRow

	models := make([]StringModel, 0, len(rows))
	for _, r := range rows {
		models = append(models, StringModel{
			Page:      r.Page,
			Key:       r.Key,
			Lang:      r.Lang,
			Value:     r.Value,
			UpdatedAt: r.UpdatedAt.UTC(),
		})
	}

	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.NewDelete().Model((*StringModel)(nil)).Where("1 = 1").Exec(ctx); err != nil {
		return err
	}

	for i := 0; i < len(models); i += maxRowsPerStmt {
		end := min(i+maxRowsPerStmt, len(models))

		chunk := models[i:end]

		// ON CONFLICT keeps last-one-wins semantics for duplicate
		// (page, key, lang) rows within a single XLSX.
		_, err := tx.NewInsert().
			Model(&chunk).
			On("CONFLICT (page, key, lang) DO UPDATE").
			Set("value = EXCLUDED.value").
			Set("updated_at = EXCLUDED.updated_at").
			Exec(ctx)
		if err != nil {
			return err
		}
	}

	meta := []MetaModel{
		{K: metaKeyLastHash, V: hash},
		{K: metaKeyLastPull, V: pulledAt.UTC().Format(time.RFC3339)},
		{K: metaKeyLastImportRows, V: strconv.Itoa(len(models))},
	}
	for _, m := range meta {
		if err := upsertMetaTx(ctx, tx, m); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func upsertMetaTx(ctx context.Context, tx bun.Tx, m MetaModel) error {
	_, err := tx.NewInsert().
		Model(&m).
		On("CONFLICT (k) DO UPDATE").
		Set("v = EXCLUDED.v").
		Exec(ctx)
	return err
}

func (db *DB) GetStringsByPagesLang(ctx context.Context, pages []string, lang string) (map[string]map[string]string, []string, error) {
	cleaned := make([]string, 0, len(pages))
	seen := make(map[string]struct{}, len(pages))

	for _, p := range pages {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		cleaned = append(cleaned, p)
	}

	out := make(map[string]map[string]string, len(cleaned))
	for _, p := range cleaned {
		out[p] = map[string]string{}
	}
	if len(cleaned) == 0 {
		return out, cleaned, nil
	}

	var rows []StringModel
	err := db.bun.NewSelect().
		Model(&rows).
		Column("page", "key", "value").
		Where("lang = ?", lang).
		Where("page IN (?)", bun.In(cleaned)).
		OrderExpr("page ASC, key ASC").
		Scan(ctx)

	if err != nil {
		return nil, nil, err
	}

	for _, r := range rows {
		out[r.Page][r.Key] = r.Value
	}

	return out, cleaned, nil
}

func (db *DB) SetMeta(ctx context.Context, k, v string) error {
	m := &MetaModel{K: k, V: v}

	_, err := db.bun.NewInsert().
		Model(m).
		On("CONFLICT (k) DO UPDATE").
		Set("v = EXCLUDED.v").
		Exec(ctx)

	return err
}

func (db *DB) GetMeta(ctx context.Context, k string) (string, bool, error) {
	m := new(MetaModel)
	err := db.bun.NewSelect().
		Model(m).
		Where("k = ?", k).
		Limit(1).
		Scan(ctx)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}

	return m.V, true, nil
}
