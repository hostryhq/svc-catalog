package service

import (
	"encoding/csv"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hostryhq/svc-catalog/internal/model"
)

// MaxImportRows is the row limit of one import (RM-M9).
const MaxImportRows = 2000

// maxReportedErrors is how many row errors a preview returns; error_count still counts them all.
const maxReportedErrors = 100

// Import row error codes (stable; the owner app shows them next to the row).
const (
	ErrRequired     = "required"
	ErrTooLong      = "too_long"
	ErrCodeInvalid  = "code_invalid"
	ErrPriceInvalid = "price_invalid"
	ErrVegInvalid   = "veg_mark_invalid"
	ErrItemConflict = "item_conflict"
	ErrVariantDup   = "variant_duplicate"
)

// csvRow is one data row.
type csvRow struct {
	n                                                  int // 1-based, header excluded
	category, name, code, variant, price, veg, station string
}

var pricePat = regexp.MustCompile(`^[0-9]{1,9}(\.[0-9]{1,2})?$`)
var codePat = regexp.MustCompile(`^[0-9]{1,8}$`)

var headerAliases = map[string]string{
	"category": "category", "item name": "name", "item_name": "name", "name": "name", "item code": "code", "item_code": "code", "code": "code",
	"variant name": "variant", "variant_name": "variant", "variant": "variant", "price": "price", "veg mark": "veg", "veg_mark": "veg", "veg": "veg",
	"station": "station",
}

// parseCSV reads the file. A file-level problem (not UTF-8, no header, a required column missing, too many
// rows) is a validation error of the whole request; row-level problems are returned by validateRows.
func parseCSV(text string) ([]csvRow, error) {
	if !utf8.ValidString(text) {
		return nil, model.Invalid("csv_text", "not_utf8", "the file must be UTF-8")
	}
	text = strings.TrimPrefix(text, "\xef\xbb\xbf") // UTF-8 byte order mark
	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	head, err := r.Read()
	if err != nil {
		return nil, model.Invalid("csv_text", "csv_empty", "the file has no header row")
	}
	col := map[string]int{}
	for i, h := range head {
		if k, ok := headerAliases[strings.ToLower(strings.TrimSpace(h))]; ok {
			if _, dup := col[k]; !dup {
				col[k] = i
			}
		}
	}
	for _, need := range []string{"category", "name", "price"} {
		if _, ok := col[need]; !ok {
			return nil, model.Invalid("csv_text", "csv_header_invalid", "the header needs the columns category, item name and price")
		}
	}
	get := func(rec []string, k string) string {
		if i, ok := col[k]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var rows []csvRow
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, model.Invalid("csv_text", "csv_malformed", "the file is not valid CSV")
		}
		if len(rows) >= MaxImportRows {
			return nil, model.Invalid("csv_text", "too_many_rows", "at most "+strconv.Itoa(MaxImportRows)+" rows")
		}
		rows = append(rows, csvRow{n: len(rows) + 1, category: get(rec, "category"), name: get(rec, "name"), code: get(rec, "code"),
			variant: get(rec, "variant"), price: get(rec, "price"), veg: get(rec, "veg"), station: get(rec, "station")})
	}
	if len(rows) == 0 {
		return nil, model.Invalid("csv_text", "csv_empty", "the file has no data rows")
	}
	return rows, nil
}

// priceToPaise converts rupees with up to two decimals to paise without floating point.
func priceToPaise(s string) (int64, bool) {
	if !pricePat.MatchString(s) {
		return 0, false
	}
	whole, frac, _ := strings.Cut(s, ".")
	for len(frac) < 2 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, false
	}
	f, _ := strconv.ParseInt(frac, 10, 64)
	return w*100 + f, true
}

func normVeg(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer("-", "_", " ", "_").Replace(s)
	switch s {
	case "":
		return "none", true
	case "veg", "non_veg", "egg", "none":
		return s, true
	case "nonveg":
		return "non_veg", true
	}
	return "", false
}

func runes(s string) int { return utf8.RuneCountInString(s) }

func truncate(s string, n int) string {
	if runes(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// rowErrors validates one row on its own.
func rowErrors(r csvRow) []model.ImportRowError {
	var errs []model.ImportRowError
	add := func(field, code string) {
		errs = append(errs, model.ImportRowError{Row: r.n, Field: field, Code: code})
	}
	if r.category == "" {
		add("category", ErrRequired)
	} else if runes(r.category) > 60 {
		add("category", ErrTooLong)
	}
	if r.name == "" {
		add("item name", ErrRequired)
	} else if runes(r.name) > 80 {
		add("item name", ErrTooLong)
	}
	if r.code != "" && !codePat.MatchString(r.code) {
		add("item code", ErrCodeInvalid)
	}
	if runes(r.variant) > 40 {
		add("variant name", ErrTooLong)
	}
	if r.price == "" {
		add("price", ErrRequired)
	} else if _, ok := priceToPaise(r.price); !ok {
		add("price", ErrPriceInvalid)
	}
	if _, ok := normVeg(r.veg); !ok {
		add("veg mark", ErrVegInvalid)
	}
	if runes(r.station) > 20 {
		add("station", ErrTooLong)
	}
	return errs
}
