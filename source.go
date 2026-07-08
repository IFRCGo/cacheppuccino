package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// maxXLSXBytes caps downloads so a wrong URL (a huge file or an endless
// stream) fails loudly instead of exhausting memory.
const maxXLSXBytes = 50 << 20

// XLSXSource provides the translation XLSX export bytes.
type XLSXSource interface {
	// Name identifies the source kind ("api" or "url") for /status and logs.
	Name() string
	Fetch(ctx context.Context, logger *slog.Logger) ([]byte, error)
}

// URLSource fetches the XLSX from a plain HTTP(S) URL. It mocks the
// translation service on QA/alpha instances (TRANSLATION_SOURCE=url):
// QA hosts a file in the same format the real service exports, and the
// regular pull loop picks up changes.
//
// The URL may carry credentials in its query string (SAS/presigned URLs),
// so logs and returned errors only ever use the redacted form: pull errors
// end up on the unauthenticated /status endpoint.
type URLSource struct {
	url         string
	redactedURL string
	http        *http.Client
}

func NewURLSource(cfg Config) *URLSource {
	return &URLSource{
		url:         cfg.TranslationXLSXURL,
		redactedURL: redactURL(cfg.TranslationXLSXURL),
		http: &http.Client{
			Timeout: cfg.HTTPTimeout,
		},
	}
}

func (s *URLSource) Name() string { return "url" }

func (s *URLSource) Fetch(ctx context.Context, logger *slog.Logger) ([]byte, error) {
	logger.Info("pull: requesting xlsx", slog.String("url", s.redactedURL))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		// The wrapped error would embed the raw URL; keep it out.
		return nil, fmt.Errorf("xlsx request failed: invalid URL %s", s.redactedURL)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		// Transport errors (*url.Error) embed the full URL including any
		// query credentials; rebuild the message around the redacted URL.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return nil, fmt.Errorf("xlsx request failed: %s %s: %w", urlErr.Op, s.redactedURL, urlErr.Err)
		}
		return nil, fmt.Errorf("xlsx request failed: GET %s", s.redactedURL)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		// Error bodies from blob stores can echo signature parameters.
		return nil, fmt.Errorf("download failed: %s: %s", resp.Status, s.redactSecrets(string(b)))
	}

	return readAllLimited(resp.Body, maxXLSXBytes)
}

// redactURL strips query, fragment, and userinfo (where SAS tokens and
// presigned signatures live) so the URL is safe for logs and /status.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid url>"
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String()
}

// redactSecrets masks the URL's query parameter values in msg, in case an
// upstream error body echoes them back.
func (s *URLSource) redactSecrets(msg string) string {
	u, err := url.Parse(s.url)
	if err != nil {
		return msg
	}
	for _, values := range u.Query() {
		for _, v := range values {
			if len(v) >= 8 {
				msg = strings.ReplaceAll(msg, v, "***")
			}
		}
	}
	return msg
}

// readAllLimited reads r fully, erroring if it exceeds max bytes.
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return b, nil
}
