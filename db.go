package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/sqliteshim"
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

	sqldb, err := sql.Open(
		sqliteshim.ShimName,
		"file:"+path+"?mode=rwc"+
			"&_pragma=busy_timeout(5000)"+
			"&_pragma=temp_store=2"+
			"&_pragma=busy_timeout(5000)",
	)

	if err != nil {
		return nil, err
	}

	sqldb.SetMaxOpenConns(1)
	sqldb.SetConnMaxLifetime(0)

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

func (db *DB) UpsertString(ctx context.Context, page, key, lang, value string, updatedAt time.Time) error {
	m := &StringModel{
		Page:      page,
		Key:       key,
		Lang:      lang,
		Value:     value,
		UpdatedAt: updatedAt.UTC(),
	}

	_, err := db.bun.NewInsert().
		Model(m).
		On("CONFLICT (page, key, lang) DO UPDATE").
		Set("value = EXCLUDED.value").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)

	return err
}

// UpsertStringsBatch upserts many rows efficiently.
// Uses a transaction + chunked multi-row INSERT to avoid SQLite variable limits.
func (db *DB) UpsertStringsBatch(ctx context.Context, rows []StringModel) error {
	if len(rows) == 0 {
		return nil
	}

	// SQLite default max variables is often 999.
	// We insert 5 columns per row: page, key, lang, value, updated_at.
	const (
		sqliteMaxVars  = 999
		colsPerRow     = 5
		safetyHeadroom = 50 // leave some room
	)
	maxRowsPerStmt := (sqliteMaxVars - safetyHeadroom) / colsPerRow
	if maxRowsPerStmt < 1 {
		return fmt.Errorf("invalid maxRowsPerStmt=%d", maxRowsPerStmt)
	}

	// Transaction: makes the whole import much faster and consistent.
	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for i := 0; i < len(rows); i += maxRowsPerStmt {
		end := min(i+maxRowsPerStmt, len(rows))

		chunk := rows[i:end]

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

	return tx.Commit()
}

// UpsertStringsFromRows is a convenience wrapper if you want to pass parsed rows directly.
func (db *DB) UpsertStringsFromRows(
	ctx context.Context,
	rows []StringRow, // rename ParsedRow to your actual ParseXLSX row type
) error {
	if len(rows) == 0 {
		return nil
	}

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

	return db.UpsertStringsBatch(ctx, models)
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
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, err
	}

	return m.V, true, nil
}
