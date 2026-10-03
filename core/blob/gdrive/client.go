// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gdrive implements Suchi's immutable blob contract on Google Drive.
package gdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	defaultAPIBaseURL      = "https://www.googleapis.com/drive/v3"
	defaultUploadBaseURL   = "https://www.googleapis.com/upload/drive/v3"
	defaultTokenURL        = "https://oauth2.googleapis.com/token"
	folderMIMEType         = "application/vnd.google-apps.folder"
	namespaceProperty      = "suchiBlobNamespace"
	namespacePropertyValue = "v1"
	blobProperty           = "suchiBlobSHA256"
	aetherStorageKey       = "aetherStorageKey"
	maxJSONResponse        = 8 << 20
	defaultHTTPTimeout     = 5 * time.Minute
)

// Config contains the OAuth credentials and parent folder for Drive storage.
// Empty endpoint fields select Google's production endpoints.
type Config struct {
	ClientID        string
	ClientSecret    string
	RefreshToken    string
	FolderID        string
	HTTPClient      *http.Client
	APIBaseURL      string
	UploadBaseURL   string
	TokenURL        string
	CreateNamespace bool
}

type driveClient struct {
	client       *http.Client
	tokenHTTP    *http.Client
	oauth        *oauth2.Config
	refreshToken string
	apiBase      *url.URL
	uploadBase   *url.URL
	tokenMu      sync.Mutex
	token        *oauth2.Token
}

type driveFile struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	MIMEType      string            `json:"mimeType"`
	Parents       []string          `json:"parents"`
	Trashed       bool              `json:"trashed"`
	AppProperties map[string]string `json:"appProperties"`
	Size          json.RawMessage   `json:"size"`
	SHA256        string            `json:"sha256Checksum"`
	ModifiedTime  string            `json:"modifiedTime"`
}

type filePage struct {
	Files         []driveFile `json:"files"`
	NextPageToken string      `json:"nextPageToken"`
}

type apiStatusError struct {
	status int
}

func (e apiStatusError) Error() string {
	switch e.status {
	case http.StatusUnauthorized:
		return "Google Drive rejected the OAuth token (401)"
	case http.StatusForbidden:
		return "Google Drive denied access or quota (403)"
	case http.StatusNotFound:
		return "Google Drive object not found (404)"
	case http.StatusTooManyRequests:
		return "Google Drive rate limit exceeded (429)"
	default:
		if e.status >= 500 {
			return fmt.Sprintf("Google Drive service error (%d)", e.status)
		}
		return fmt.Sprintf("Google Drive request failed (%d)", e.status)
	}
}

func newDriveClient(config Config) (*driveClient, error) {
	if strings.TrimSpace(config.ClientID) == "" || strings.TrimSpace(config.ClientSecret) == "" ||
		strings.TrimSpace(config.RefreshToken) == "" || strings.TrimSpace(config.FolderID) == "" {
		return nil, errors.New("Google Drive requires client ID, client secret, refresh token, and parent folder ID")
	}

	apiURL, err := parseBaseURL(config.APIBaseURL, defaultAPIBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Google Drive API base URL: %w", err)
	}
	uploadURL, err := parseBaseURL(config.UploadBaseURL, defaultUploadBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Google Drive upload base URL: %w", err)
	}
	tokenURL, err := parseEndpointURL(config.TokenURL, defaultTokenURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Google OAuth token URL: %w", err)
	}

	baseClient := config.HTTPClient
	if baseClient == nil {
		baseClient = http.DefaultClient
	}
	client := sameOriginRedirectClient(baseClient)
	oauthConfig := &oauth2.Config{
		ClientID:     config.ClientID,
		ClientSecret: config.ClientSecret,
		Endpoint: oauth2.Endpoint{
			TokenURL:  tokenURL.String(),
			AuthStyle: oauth2.AuthStyleAutoDetect,
		},
	}
	return &driveClient{
		client:       client,
		tokenHTTP:    client,
		oauth:        oauthConfig,
		refreshToken: config.RefreshToken,
		apiBase:      apiURL,
		uploadBase:   uploadURL,
	}, nil
}

func parseBaseURL(raw, fallback string) (*url.URL, error) {
	if raw == "" {
		raw = fallback
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("expected an absolute HTTP(S) URL without user info, query, or fragment")
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("HTTP is allowed only for loopback test endpoints")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func parseEndpointURL(raw, fallback string) (*url.URL, error) {
	if raw == "" {
		raw = fallback
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("expected an absolute HTTP(S) URL without user info, query, or fragment")
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("HTTP is allowed only for loopback test endpoints")
	}
	return u, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameOriginRedirectClient(base *http.Client) *http.Client {
	client := *base
	if client.Timeout == 0 {
		client.Timeout = defaultHTTPTimeout
	}
	original := base.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 0 && !sameOrigin(via[0].URL, request.URL) {
			return errors.New("refusing off-origin HTTP redirect")
		}
		if original != nil {
			return original(request, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &client
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func (c *driveClient) accessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != nil && c.token.Valid() {
		return c.token.AccessToken, nil
	}
	seed := c.token
	if seed == nil {
		seed = &oauth2.Token{RefreshToken: c.refreshToken}
	}
	tokenContext := context.WithValue(ctx, oauth2.HTTPClient, c.tokenHTTP)
	token, err := c.oauth.TokenSource(tokenContext, seed).Token()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// OAuth response bodies can echo request details. Keep diagnostics free of
		// credential values by intentionally omitting the underlying error.
		return "", errors.New("refresh Google Drive OAuth access token failed")
	}
	if token.AccessToken == "" {
		return "", errors.New("refresh Google Drive OAuth access token returned an empty token")
	}
	c.token = token
	return token.AccessToken, nil
}

func (c *driveClient) request(ctx context.Context, method, target string, body io.Reader, headers http.Header) (*http.Response, error) {
	accessToken, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, errors.New("build Google Drive request")
	}
	if headers == nil {
		headers = make(http.Header)
	}
	req.Header = headers.Clone()
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("Google Drive request failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, httpStatusError(resp.StatusCode)
	}
	return resp, nil
}

func httpStatusError(status int) error {
	return apiStatusError{status: status}
}

func (c *driveClient) apiURL(path string, query url.Values) string {
	u := *c.apiBase
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = query.Encode()
	return u.String()
}

func (c *driveClient) uploadURL(path string, query url.Values) string {
	u := *c.uploadBase
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = query.Encode()
	return u.String()
}

func fileURLPath(id string) string {
	return "/files/" + url.PathEscape(id)
}

func (c *driveClient) getJSON(ctx context.Context, target string, out any) error {
	resp, err := c.request(ctx, http.MethodGet, target, nil, make(http.Header))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONResponse)).Decode(out); err != nil {
		return errors.New("invalid JSON from Google Drive")
	}
	return nil
}

func (f driveFile) size() (int64, error) {
	if len(f.Size) == 0 || string(f.Size) == "null" {
		return 0, errors.New("Google Drive object omitted its size")
	}
	var text string
	if f.Size[0] == '"' {
		if err := json.Unmarshal(f.Size, &text); err != nil {
			return 0, errors.New("Google Drive object has an invalid size")
		}
	} else {
		text = string(f.Size)
	}
	size, err := strconv.ParseInt(text, 10, 64)
	if err != nil || size < 0 {
		return 0, errors.New("Google Drive object has an invalid size")
	}
	return size, nil
}

func (f driveFile) modified() (time.Time, error) {
	if f.ModifiedTime == "" {
		return time.Time{}, errors.New("Google Drive object omitted its modified time")
	}
	modified, err := time.Parse(time.RFC3339Nano, f.ModifiedTime)
	if err != nil {
		return time.Time{}, errors.New("Google Drive object has an invalid modified time")
	}
	return modified, nil
}

func hasParent(file driveFile, parentID string) bool {
	for _, parent := range file.Parents {
		if parent == parentID {
			return true
		}
	}
	return false
}

func escapeDriveQuery(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	return strings.ReplaceAll(value, "'", "\\'")
}
