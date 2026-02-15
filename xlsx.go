package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

type StringRow struct {
	Page      string
	Key       string
	Lang      string
	Value     string
	UpdatedAt time.Time
}

func HashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// XLSX format:
// Sheet: "Translations"
// Header: page | key | ar | en | es | fr | ...
func ParseXLSX(xlsx []byte) ([]StringRow, error) {
	f, err := excelize.OpenReader(bytes.NewReader(xlsx))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	sheet := "Translations"
	if !sheetExists(f, sheet) {
		sheets := f.GetSheetList()
		if len(sheets) == 0 {
			return nil, fmt.Errorf("xlsx has no sheets")
		}
		sheet = sheets[0]
	}

	rows, err := f.GetRows(sheet)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	header := rows[0]
	if len(header) < 3 {
		return nil, fmt.Errorf("expected at least 3 columns (page, key, <langs...>)")
	}

	if strings.ToLower(strings.TrimSpace(header[0])) != "page" ||
		strings.ToLower(strings.TrimSpace(header[1])) != "key" {
		return nil, fmt.Errorf("unexpected header: first two columns must be 'page' and 'key'")
	}

	langs := make([]string, 0, len(header)-2)
	for _, h := range header[2:] {
		langs = append(langs, strings.ToLower(strings.TrimSpace(h)))
	}

	now := time.Now().UTC()
	out := make([]StringRow, 0)

	for _, row := range rows[1:] {
		if len(row) < 2 {
			continue
		}

		page := strings.TrimSpace(row[0])
		key := strings.TrimSpace(row[1])

		if page == "" || key == "" {
			continue
		}

		for i, lang := range langs {
			if lang == "" {
				continue
			}

			colIdx := i + 2
			if colIdx >= len(row) {
				continue
			}

			val := strings.TrimSpace(row[colIdx])
			if val == "" {
				continue
			}

			out = append(out, StringRow{
				Page:      page,
				Key:       key,
				Lang:      lang,
				Value:     val,
				UpdatedAt: now,
			})
		}
	}

	return out, nil
}

func sheetExists(f *excelize.File, name string) bool {
	for _, s := range f.GetSheetList() {
		if s == name {
			return true
		}
	}
	return false
}
