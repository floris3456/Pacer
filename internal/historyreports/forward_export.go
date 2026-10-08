package historyreports

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// Buffer before writing so errors/limits cannot produce a successful truncated
// download. These library functions do not expose or register an HTTP route.
func (s *ForwardStore) ExportForwardJSON(ctx context.Context, p ForwardPin, w io.Writer) error {
	if w == nil {
		return ErrInput
	}
	r, err := s.ForwardReport(ctx, p)
	if err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b)+1 > 8<<20 {
		return ErrLimit
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	var n int
	n, err = w.Write(append(b, '\n'))
	if err == nil && n != len(b)+1 {
		return io.ErrShortWrite
	}
	return err
}
func forwardCSVSafe(v string) string {
	trim := strings.TrimLeft(v, " \t\r\n")
	if trim != "" && strings.ContainsRune("=+-@", rune(trim[0])) {
		return "'" + v
	}
	return v
}
func (s *ForwardStore) ExportForwardCSV(ctx context.Context, p ForwardPin, limit int, cursor string, w io.Writer) (string, error) {
	if w == nil {
		return "", ErrInput
	}
	page, err := s.ForwardRequests(ctx, p, limit, cursor)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	c := csv.NewWriter(&b)
	if err = c.Write([]string{"event_id", "account", "source_records", "identity_resolved", "low_nano_usd", "high_nano_usd", "pin_json", "evidence_json"}); err != nil {
		return "", err
	}
	pin, _ := json.Marshal(page.Pin)
	for _, e := range page.Rows {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		low, high := "", ""
		if e.Value != nil {
			low = strconv.FormatInt(e.Value.Low, 10)
			high = strconv.FormatInt(e.Value.High, 10)
		}
		raw, _ := json.Marshal(e)
		if err = c.Write([]string{forwardCSVSafe(e.ID), forwardCSVSafe(e.Account), strconv.Itoa(len(e.Origins)), strconv.FormatBool(e.IdentityResolved), low, high, string(pin), string(raw)}); err != nil {
			return "", err
		}
		c.Flush()
		if err = c.Error(); err != nil {
			return "", err
		}
		if b.Len() > 8<<20 {
			return "", ErrLimit
		}
	}
	c.Flush()
	if err = c.Error(); err != nil {
		return "", err
	}
	var n int
	n, err = w.Write(b.Bytes())
	if err == nil && n != b.Len() {
		return "", io.ErrShortWrite
	}
	return page.Next, err
}
