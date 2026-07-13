package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/xuri/excelize/v2"
)

// discardLogger returns a logger that swallows all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// openTestDB opens a fresh SQLite database under t.TempDir() and closes it
// on cleanup.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// xlsxSheet describes one sheet of an in-memory test workbook.
type xlsxSheet struct {
	name string
	rows [][]any
}

// buildXLSX builds an in-memory workbook. The first sheet replaces the
// default "Sheet1"; subsequent sheets are appended.
func buildXLSX(t *testing.T, sheets []xlsxSheet) []byte {
	t.Helper()

	f := excelize.NewFile()
	defer func() { _ = f.Close() }()

	for i, sheet := range sheets {
		if i == 0 {
			if sheet.name != "Sheet1" {
				if err := f.SetSheetName("Sheet1", sheet.name); err != nil {
					t.Fatalf("SetSheetName: %v", err)
				}
			}
		} else {
			if _, err := f.NewSheet(sheet.name); err != nil {
				t.Fatalf("NewSheet: %v", err)
			}
		}
		for rowIndex, row := range sheet.rows {
			cell, err := excelize.CoordinatesToCellName(1, rowIndex+1)
			if err != nil {
				t.Fatalf("CoordinatesToCellName: %v", err)
			}
			if err := f.SetSheetRow(sheet.name, cell, &row); err != nil {
				t.Fatalf("SetSheetRow: %v", err)
			}
		}
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatalf("WriteToBuffer: %v", err)
	}
	return buf.Bytes()
}

// translationsXLSX builds a workbook with a single "Translations" sheet;
// rows[0] is the header.
func translationsXLSX(t *testing.T, rows [][]string) []byte {
	t.Helper()

	anyRows := make([][]any, 0, len(rows))
	for _, row := range rows {
		anyRow := make([]any, len(row))
		for i, cell := range row {
			anyRow[i] = cell
		}
		anyRows = append(anyRows, anyRow)
	}
	return buildXLSX(t, []xlsxSheet{{name: "Translations", rows: anyRows}})
}
