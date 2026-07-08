package main

import (
	"regexp"
	"testing"
	"time"
)

// rowContentEq compares everything except UpdatedAt.
func rowContentEq(a, b StringRow) bool {
	return a.Page == b.Page && a.Key == b.Key && a.Lang == b.Lang && a.Value == b.Value
}

func assertRows(t *testing.T, got, want []StringRow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d\ngot: %+v", len(got), len(want), got)
	}
	for i := range want {
		if !rowContentEq(got[i], want[i]) {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseXLSXHappyPath(t *testing.T) {
	// A decoy first sheet with an invalid header ensures the
	// "Translations" sheet is preferred over the first sheet.
	data := buildXLSX(t, []xlsxSheet{
		{
			name: "Decoy",
			rows: [][]any{{"Namespace", "Key", "en"}, {"x", "y", "z"}},
		},
		{
			name: "Translations",
			rows: [][]any{
				{"page", "key", "EN", "fr", "Es"},
				{"home", "title", "Hello", "Bonjour", "Hola"},
				{"home", "subtitle", "World", "", "Mundo"},
			},
		},
	})

	before := time.Now().UTC()
	got, err := ParseXLSX(data)
	if err != nil {
		t.Fatalf("ParseXLSX: %v", err)
	}

	want := []StringRow{
		{Page: "home", Key: "title", Lang: "en", Value: "Hello"},
		{Page: "home", Key: "title", Lang: "fr", Value: "Bonjour"},
		{Page: "home", Key: "title", Lang: "es", Value: "Hola"},
		{Page: "home", Key: "subtitle", Lang: "en", Value: "World"},
		{Page: "home", Key: "subtitle", Lang: "es", Value: "Mundo"},
	}
	assertRows(t, got, want)

	for i, r := range got {
		if r.UpdatedAt.IsZero() {
			t.Errorf("row %d: UpdatedAt is zero", i)
		}
		if r.UpdatedAt.Location() != time.UTC {
			t.Errorf("row %d: UpdatedAt location = %v, want UTC", i, r.UpdatedAt.Location())
		}
		if r.UpdatedAt.Before(before) || r.UpdatedAt.After(time.Now().UTC()) {
			t.Errorf("row %d: UpdatedAt %v outside expected window", i, r.UpdatedAt)
		}
		if !r.UpdatedAt.Equal(got[0].UpdatedAt) {
			t.Errorf("row %d: UpdatedAt differs from first row", i)
		}
	}
}

func TestParseXLSXSheetFallback(t *testing.T) {
	// No "Translations" sheet: the first sheet is parsed instead.
	data := buildXLSX(t, []xlsxSheet{
		{
			name: "Sheet1",
			rows: [][]any{
				{"page", "key", "en"},
				{"about", "heading", "About us"},
			},
		},
	})

	got, err := ParseXLSX(data)
	if err != nil {
		t.Fatalf("ParseXLSX: %v", err)
	}
	assertRows(t, got, []StringRow{
		{Page: "about", Key: "heading", Lang: "en", Value: "About us"},
	})
}

func TestParseXLSXHeaderValidation(t *testing.T) {
	tests := []struct {
		name    string
		header  []any
		wantErr bool
	}{
		{"namespace instead of page", []any{"Namespace", "Key", "en"}, true},
		{"swapped page and key", []any{"key", "page", "en"}, true},
		{"fewer than 3 columns", []any{"page", "key"}, true},
		{"case-insensitive with whitespace", []any{" Page ", " KEY ", " en"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := buildXLSX(t, []xlsxSheet{
				{
					name: "Translations",
					rows: [][]any{tt.header, {"p1", "k1", "v1"}},
				},
			})
			got, err := ParseXLSX(data)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseXLSX: expected error, got rows %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseXLSX: %v", err)
			}
			assertRows(t, got, []StringRow{
				{Page: "p1", Key: "k1", Lang: "en", Value: "v1"},
			})
		})
	}
}

func TestParseXLSXRowFiltering(t *testing.T) {
	data := buildXLSX(t, []xlsxSheet{
		{
			name: "Translations",
			rows: [][]any{
				{"page", "key", "en", "", "fr"},    // empty lang header: column skipped
				{"   ", "k1", "skipped"},           // whitespace page: row skipped
				{"p1", "  ", "skipped"},            // whitespace key: row skipped
				{"onlypage"},                       // shorter than 2 cells: row skipped
				{"p2", "k2"},                       // no value cells: nothing emitted
				{"p3", "k3", "", "x", "  salut  "}, // empty en cell + skipped empty-header col
				{"p4", "k4", "  hello  "},          // value gets trimmed
			},
		},
	})

	got, err := ParseXLSX(data)
	if err != nil {
		t.Fatalf("ParseXLSX: %v", err)
	}
	assertRows(t, got, []StringRow{
		{Page: "p3", Key: "k3", Lang: "fr", Value: "salut"},
		{Page: "p4", Key: "k4", Lang: "en", Value: "hello"},
	})
}

func TestParseXLSXEmptySheet(t *testing.T) {
	data := buildXLSX(t, []xlsxSheet{{name: "Translations"}})

	got, err := ParseXLSX(data)
	if err != nil {
		t.Fatalf("ParseXLSX: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil rows for empty sheet, got %+v", got)
	}
}

func TestParseXLSXInvalidInput(t *testing.T) {
	if _, err := ParseXLSX([]byte("junk")); err == nil {
		t.Fatal("expected error for non-xlsx input")
	}
}

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestHashBytes(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{
			"empty input",
			[]byte{},
			"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			"abc",
			[]byte("abc"),
			"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HashBytes(tt.input)
			if !sha256HexRe.MatchString(got) {
				t.Errorf("HashBytes(%q) = %q, not 64 lowercase hex chars", tt.input, got)
			}
			if got != tt.want {
				t.Errorf("HashBytes(%q) = %q, want %q", tt.input, got, tt.want)
			}
			if again := HashBytes(tt.input); again != got {
				t.Errorf("HashBytes not deterministic: %q then %q", got, again)
			}
		})
	}

	if HashBytes([]byte("a")) == HashBytes([]byte("b")) {
		t.Error("HashBytes returned same hash for different inputs")
	}
}
