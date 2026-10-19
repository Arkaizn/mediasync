package jellyfin

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/redact"
)

// maxResponseBytes bounds response bodies we read.
const maxResponseBytes = 8 << 20

// Client is a minimal Jellyfin HTTP API client.
//
// Authentication uses only the Authorization header
// (`MediaBrowser Client=…, Device=…, DeviceId=…, Version=…, Token=…`).
// Jellyfin 12 rejects the legacy X-Emby-Token header and api_key query
// parameter, and credentials must never appear in URLs anyway.
type Client struct {
	base       *url.URL
	http       *http.Client
	clientName string
	version    string
	deviceName string
	deviceID   string

	mu    sync.RWMutex
	token redact.Secret
}

// ClientOptions configure a Client.
type ClientOptions struct {
	BaseURL    string
	DeviceID   string
	DeviceName string
	Version    string
	Timeout    time.Duration
	// CAFile adds a PEM CA bundle to the system roots (for private CAs).
	// Certificate verification is never disabled.
	CAFile string
	// HTTPClient overrides the HTTP client (tests).
	HTTPClient *http.Client
}

// ParseServerURL validates and normalises a server URL.
func ParseServerURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, model.Errorf(model.ErrInvalidConfig, "invalid Jellyfin server URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, model.Errorf(model.ErrInvalidConfig, "Jellyfin server URL must use http or https")
	}
	if u.User != nil {
		return nil, model.Errorf(model.ErrInvalidConfig, "Jellyfin server URL must not contain credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, model.Errorf(model.ErrInvalidConfig, "Jellyfin server URL must not contain a query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

// NewClient creates a client.
func NewClient(o ClientOptions) (*Client, error) {
	base, err := ParseServerURL(o.BaseURL)
	if err != nil {
		return nil, err
	}
	hc := o.HTTPClient
	if hc == nil {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if o.CAFile != "" {
			pem, err := os.ReadFile(o.CAFile)
			if err != nil {
				return nil, model.Wrap(model.ErrInvalidConfig, err, "cannot read ca_file")
			}
			pool, err := x509.SystemCertPool()
			if err != nil || pool == nil {
				pool = x509.NewCertPool()
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, model.Errorf(model.ErrInvalidConfig, "ca_file contains no PEM certificates")
			}
			tlsCfg.RootCAs = pool
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = tlsCfg
		tr.MaxIdleConnsPerHost = 4
		tr.ResponseHeaderTimeout = o.Timeout
		hc = &http.Client{
			Transport: tr,
			Timeout:   o.Timeout,
			// Never follow redirects to a different host with our credentials.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 0 && req.URL.Host != via[0].URL.Host {
					return errors.New("refusing cross-host redirect")
				}
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				return nil
			},
		}
	}
	if o.Version == "" {
		o.Version = "0"
	}
	if o.DeviceName == "" {
		o.DeviceName = "mediasync"
	}
	return &Client{
		base: base, http: hc, clientName: "mediasync",
		version: o.Version, deviceName: o.DeviceName, deviceID: o.DeviceID,
	}, nil
}

// BaseURL returns the normalised server URL.
func (c *Client) BaseURL() string { return c.base.String() }

// SetToken sets the access token used for requests.
func (c *Client) SetToken(t redact.Secret) {
	c.mu.Lock()
	c.token = t
	c.mu.Unlock()
}

// HasToken reports whether a token is set.
func (c *Client) HasToken() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token != ""
}

func headerValue(s string) string {
	// Jellyfin URL-decodes header parameter values; encode anything that
	// could break the quoted format.
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func (c *Client) authHeader() string {
	h := fmt.Sprintf(`MediaBrowser Client="%s", Device="%s", DeviceId="%s", Version="%s"`,
		headerValue(c.clientName), headerValue(c.deviceName), headerValue(c.deviceID), headerValue(c.version))
	c.mu.RLock()
	tok := c.token
	c.mu.RUnlock()
	if tok != "" {
		h += fmt.Sprintf(`, Token="%s"`, tok.Reveal())
	}
	return h
}

// apiError builds a structured error from an HTTP failure. It only includes
// the request path (never query strings or headers).
func apiError(path string, status int, notFound model.ErrorCode) error {
	switch status {
	case http.StatusUnauthorized:
		return model.Errorf(model.ErrAuthFailed, "Jellyfin rejected the credentials (HTTP 401 on %s)", path).WithData("http_status", status)
	case http.StatusForbidden:
		return model.Errorf(model.ErrAuthFailed, "Jellyfin denied access (HTTP 403 on %s)", path).WithData("http_status", status)
	case http.StatusNotFound:
		return model.Errorf(notFound, "not found (HTTP 404 on %s)", path).WithData("http_status", status)
	case http.StatusBadRequest:
		return model.Errorf(model.ErrProviderError, "Jellyfin rejected the request (HTTP 400 on %s)", path).WithData("http_status", status)
	default:
		return model.Errorf(model.ErrProviderError, "Jellyfin returned HTTP %d on %s", status, path).WithData("http_status", status)
	}
}

func transportError(err error, path string) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuth) || errors.As(err, &hostErr) || errors.As(err, &invalid) {
		return model.Wrap(model.ErrTLS, err, "TLS certificate verification failed for the Jellyfin server (configure ca_file for a private CA)")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return model.Wrap(model.ErrTimeout, err, "request to Jellyfin timed out ("+path+")")
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return model.Wrap(model.ErrTimeout, err, "request to Jellyfin timed out ("+path+")")
	}
	return model.Wrap(model.ErrServerUnreachable, err, "cannot reach the Jellyfin server ("+path+")")
}

// do performs a request. out may be nil. notFound is the error code used
// for HTTP 404.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any, notFound model.ErrorCode) error {
	_, err := c.doHeader(ctx, method, path, query, body, out, notFound)
	return err
}

// doHeader is do but also returns the response headers.
func (c *Client) doHeader(ctx context.Context, method, path string, query url.Values, body any, out any, notFound model.ErrorCode) (http.Header, error) {
	u := *c.base
	u.Path = c.base.Path + path
	if query != nil {
		u.RawQuery = query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, model.Wrap(model.ErrInternal, err, "cannot encode request")
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, model.Wrap(model.ErrInternal, err, "cannot build request")
	}
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(err, path)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return resp.Header, apiError(path, resp.StatusCode, notFound)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return resp.Header, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return resp.Header, model.Wrap(model.ErrProviderError, err, "invalid JSON from Jellyfin ("+path+")")
	}
	return resp.Header, nil
}

// PublicSystemInfo is /System/Info/Public.
type PublicSystemInfo struct {
	ServerName             string `json:"ServerName"`
	Version                string `json:"Version"`
	ProductName            string `json:"ProductName"`
	ID                     string `json:"Id"`
	StartupWizardCompleted *bool  `json:"StartupWizardCompleted"`
}

// PublicInfo fetches unauthenticated server information.
func (c *Client) PublicInfo(ctx context.Context) (PublicSystemInfo, error) {
	var info PublicSystemInfo
	err := c.do(ctx, http.MethodGet, "/System/Info/Public", nil, nil, &info, model.ErrProviderError)
	if err == nil && info.ID == "" {
		err = model.Errorf(model.ErrProviderError, "the server did not identify itself as Jellyfin")
	}
	return info, err
}

// User is a subset of UserDto.
type User struct {
	ID       string `json:"Id"`
	Name     string `json:"Name"`
	ServerID string `json:"ServerId"`
}

// AuthResult is the AuthenticateByName response.
type AuthResult struct {
	User        User   `json:"User"`
	AccessToken string `json:"AccessToken"`
	ServerID    string `json:"ServerId"`
}

// AuthenticateByName logs in with a user name and password. The password is
// only sent in the request body.
func (c *Client) AuthenticateByName(ctx context.Context, user string, password redact.Secret) (User, redact.Secret, error) {
	var res AuthResult
	body := map[string]string{"Username": user, "Pw": password.Reveal()}
	// No token for the login request itself.
	c.mu.Lock()
	saved := c.token
	c.token = ""
	c.mu.Unlock()
	err := c.do(ctx, http.MethodPost, "/Users/AuthenticateByName", nil, body, &res, model.ErrProviderError)
	c.mu.Lock()
	c.token = saved
	c.mu.Unlock()
	if err != nil {
		return User{}, "", err
	}
	if res.AccessToken == "" {
		return User{}, "", model.Errorf(model.ErrAuthFailed, "Jellyfin did not return an access token")
	}
	return res.User, redact.Secret(res.AccessToken), nil
}

// Me returns the authenticated user.
func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	err := c.do(ctx, http.MethodGet, "/Users/Me", nil, nil, &u, model.ErrAuthFailed)
	return u, err
}

// Logout revokes the current access token on the server.
func (c *Client) Logout(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/Sessions/Logout", nil, nil, nil, model.ErrProviderError)
}

// NameID is a {Name, Id} pair.
type NameID struct {
	Name string `json:"Name"`
	ID   string `json:"Id"`
}

// BaseItem is the subset of BaseItemDto used for now-playing items.
type BaseItem struct {
	ID                   string            `json:"Id"`
	Name                 string            `json:"Name"`
	Type                 string            `json:"Type"`
	MediaType            string            `json:"MediaType"`
	Album                string            `json:"Album"`
	AlbumID              string            `json:"AlbumId"`
	AlbumArtist          string            `json:"AlbumArtist"`
	AlbumArtists         []NameID          `json:"AlbumArtists"`
	Artists              []string          `json:"Artists"`
	RunTimeTicks         *int64            `json:"RunTimeTicks"`
	ImageTags            map[string]string `json:"ImageTags"`
	AlbumPrimaryImageTag string            `json:"AlbumPrimaryImageTag"`
	SeriesName           string            `json:"SeriesName"`
}

// PlayState is PlayerStateInfo.
type PlayState struct {
	PositionTicks *int64  `json:"PositionTicks"`
	CanSeek       bool    `json:"CanSeek"`
	IsPaused      bool    `json:"IsPaused"`
	IsMuted       bool    `json:"IsMuted"`
	VolumeLevel   *int    `json:"VolumeLevel"`
	RepeatMode    string  `json:"RepeatMode"`
	PlaybackOrder string  `json:"PlaybackOrder"`
	PlayMethod    *string `json:"PlayMethod"`
}

// SessionInfo is the subset of SessionInfoDto used by the provider.
type SessionInfo struct {
	ID                    string    `json:"Id"`
	UserID                string    `json:"UserId"`
	UserName              string    `json:"UserName"`
	Client                string    `json:"Client"`
	DeviceName            string    `json:"DeviceName"`
	DeviceID              string    `json:"DeviceId"`
	ApplicationVersion    string    `json:"ApplicationVersion"`
	RemoteEndPoint        string    `json:"RemoteEndPoint"`
	ServerID              string    `json:"ServerId"`
	IsActive              bool      `json:"IsActive"`
	SupportsMediaControl  bool      `json:"SupportsMediaControl"`
	SupportsRemoteControl bool      `json:"SupportsRemoteControl"`
	SupportedCommands     []string  `json:"SupportedCommands"`
	LastActivityDate      time.Time `json:"LastActivityDate"`
	LastPlaybackCheckIn   time.Time `json:"LastPlaybackCheckIn"`
	NowPlayingItem        *BaseItem `json:"NowPlayingItem"`
	PlayState             PlayState `json:"PlayState"`
}

// Sessions lists sessions visible to the authenticated user.
func (c *Client) Sessions(ctx context.Context) ([]SessionInfo, error) {
	var out []SessionInfo
	err := c.do(ctx, http.MethodGet, "/Sessions", nil, nil, &out, model.ErrProviderError)
	return out, err
}

// PlaystateCommand sends a playstate command (Pause, Unpause, PlayPause,
// Stop, NextTrack, PreviousTrack, Seek) to a session.
func (c *Client) PlaystateCommand(ctx context.Context, sessionID, command string, seekTicks *int64) error {
	var q url.Values
	if seekTicks != nil {
		q = url.Values{"seekPositionTicks": {strconv.FormatInt(*seekTicks, 10)}}
	}
	return c.do(ctx, http.MethodPost, "/Sessions/"+url.PathEscape(sessionID)+"/Playing/"+command, q, nil, nil, model.ErrSessionNotFound)
}

// GeneralCommand sends a general command (e.g. SetVolume) to a session.
func (c *Client) GeneralCommand(ctx context.Context, sessionID, name string, args map[string]string) error {
	body := map[string]any{"Name": name, "Arguments": args}
	return c.do(ctx, http.MethodPost, "/Sessions/"+url.PathEscape(sessionID)+"/Command", nil, body, nil, model.ErrSessionNotFound)
}

// ImageURL returns a credential-free URL for an item's primary image.
// Jellyfin serves images without authentication.
func (c *Client) ImageURL(itemID, tag string) string {
	u := *c.base
	u.Path = c.base.Path + "/Items/" + url.PathEscape(itemID) + "/Images/Primary"
	q := url.Values{}
	if tag != "" {
		q.Set("tag", tag)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
