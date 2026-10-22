package gemini

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
)

var reXSRF = regexp.MustCompile(`\["xsrf","([^"]+)"`)

type UpstreamError struct {
	Status int
	Kind   string
	Msg    string
}

func (e *UpstreamError) Error() string {
	return e.Msg
}

type Requester interface {
	Do(req *http.Request) (*http.Response, error)
}

type ModelOptions struct {
	ModelName   string
	ModelID     int
	ThinkMode   int
	ModelHeader string
	Extra       map[int]any
	IsVideo     bool
}

type Client struct {
	Cfg     config.Config
	HTTP    Requester
	Cookies *CookieCache
	Logf    func(format string, args ...any)
	blMu    sync.Mutex
	bl      string

	modelsMu      sync.RWMutex
	discovered    []DiscoveredModel
	accountCap    int
	accountCapFld int
	discMu        sync.Mutex
	lastDiscFail  time.Time
}

func NewClient(cfg config.Config) *Client {
	var req Requester
	if cfg.Impersonate != "" && cfg.Proxy == "" {
		tlsAdapter, err := getTLSClient(cfg.Impersonate, cfg.RequestTimeoutSec)
		if err != nil {
			log.Printf("Failed to create TLS client for profile %s: %v, falling back to stdlib", cfg.Impersonate, err)
		} else {
			req = tlsAdapter
		}
	}

	if req == nil {
		transport := &http.Transport{
			DisableCompression: true,
		}

		if cfg.Proxy != "" {
			if proxyURL, err := url.Parse(cfg.Proxy); err == nil {
				transport.Proxy = http.ProxyURL(proxyURL)
			}
		} else {
			transport.Proxy = http.ProxyFromEnvironment
		}

		req = &http.Client{
			Transport: transport,
			Timeout:   time.Duration(cfg.RequestTimeoutSec) * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	logFn := func(format string, args ...any) {
		if cfg.LogRequests {
			log.Printf(format, args...)
		}
	}

	return &Client{
		Cfg:     cfg,
		HTTP:    req,
		Cookies: NewCookieCache(cfg.CookieFile),
		Logf:    logFn,
		bl:      cfg.GeminiBL,
	}
}

// CurrentBL returns the current BL value, preferring the mutable field over config.
func (c *Client) CurrentBL() string {
	c.blMu.Lock()
	defer c.blMu.Unlock()
	if c.bl != "" {
		return c.bl
	}
	return c.Cfg.GeminiBL
}

// SetBL updates the BL value.
func (c *Client) SetBL(bl string) {
	c.blMu.Lock()
	defer c.blMu.Unlock()
	c.bl = bl
}

// UpdateBLIfNeeded fetches the latest BL from Gemini and updates it if different from current.
// Returns (newBL, changed, error).
func (c *Client) UpdateBLIfNeeded() (string, bool, error) {
	newBL, err := FetchLatestBL(c.HTTP)
	if err != nil {
		return "", false, err
	}
	if newBL == "" {
		return "", false, fmt.Errorf("empty BL fetched")
	}
	oldBL := c.CurrentBL()
	if newBL == oldBL {
		return newBL, false, nil
	}
	c.SetBL(newBL)
	return newBL, true, nil
}

func (c *Client) BuildHeaders() http.Header {
	return c.buildHeaders()
}

func (c *Client) buildHeaders() http.Header {
	cookieInfo, _ := c.Cookies.Load()
	authUser := c.Cfg.AuthUser
	if authUser == "" && cookieInfo.AuthUser != "" {
		authUser = cookieInfo.AuthUser
	}

	prefix := AccountPrefix(authUser)
	h := make(http.Header)
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("Origin", "https://gemini.google.com")
	h.Set("Referer", fmt.Sprintf("https://gemini.google.com%s/app", prefix))
	h.Set("X-Same-Domain", "1")
	h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	if prefix != "" && authUser != "" {
		h.Set("X-Goog-AuthUser", authUser)
	}

	if cookieInfo.Cookie != "" {
		h.Set("Cookie", cookieInfo.Cookie)
	}
	if cookieInfo.SAPISID != "" {
		hash := SAPISIDHash(cookieInfo.SAPISID)
		if hash != "" {
			h.Set("Authorization", hash)
		}
	}

	if c.Cfg.Impersonate != "" {
		h.Set("Accept-Encoding", "identity")
	}

	return h
}

func (c *Client) triageStatus(resp *http.Response) error {
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusMovedPermanently {
		return &UpstreamError{
			Status: resp.StatusCode,
			Kind:   "http",
			Msg:    "upstream blocked (redirected to sorry/index); IP may be flagged - retry later or use a different proxy/IP",
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return &UpstreamError{
			Status: resp.StatusCode,
			Kind:   "http",
			Msg:    "upstream rate limited (HTTP 429); retry later",
		}
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return &UpstreamError{
			Status: resp.StatusCode,
			Kind:   "http",
			Msg:    fmt.Sprintf("upstream HTTP %d: %s", resp.StatusCode, string(b)),
		}
	}
	return nil
}

type GenerateResult struct {
	Text           string
	ConversationID string
}

func (c *Client) Generate(prompt string, opt ModelOptions, fileRefs []string) (GenerateResult, error) {
	c.EnsureModelsDiscovered()

	cookieInfo, _ := c.Cookies.Load()
	cfg := c.Cfg
	if cfg.XSRFToken == "" && cookieInfo.XSRFToken != "" {
		cfg.XSRFToken = cookieInfo.XSRFToken
	}
	if cfg.AuthUser == "" && cookieInfo.AuthUser != "" {
		cfg.AuthUser = cookieInfo.AuthUser
	}

	reqUUID := strings.ToUpper(uuidV4())
	extra := make(map[int]any, len(opt.Extra)+1)
	for k, v := range opt.Extra {
		extra[k] = v
	}
	extra[59] = reqUUID

	modelHdr, effectiveMode := c.resolveModelHeader(opt, reqUUID)
	bodyStr := BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg, opt.IsVideo)
	reqURL := BuildURL(cfg, c.CurrentBL())
	headers := c.buildHeaders()
	headers.Set(HeaderUUIDJspb, fmt.Sprintf(`["%s",1]`, reqUUID))
	if opt.IsVideo {
		headers.Set(ModelHeaderKey, BuildVideoHeader(reqUUID, c.CurrentBL()))
		headers.Set(HeaderJspb89, "[0]")
		headers.Set(HeaderJspb90, "[0,0,0]")
	} else if modelHdr != "" {
		headers.Set(ModelHeaderKey, modelHdr)
		headers.Set(HeaderJspb89, "[0]")
		headers.Set(HeaderJspb90, "[0,0,0]")
	}

	blRefreshed := false
	var lastErr error

	maxAttempts := c.Cfg.RetryAttempts
	if total := c.Cookies.TotalAccounts(); total > maxAttempts {
		maxAttempts = total
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		req, err := http.NewRequest("POST", reqURL, strings.NewReader(bodyStr))
		if err != nil {
			return GenerateResult{}, err
		}
		req.Header = headers.Clone()

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = &UpstreamError{Kind: "transport", Msg: err.Error()}
		} else {
			if err := c.triageStatus(resp); err != nil {
				// Check if upstream 400 returned a fresh XSRF token
				if resp.StatusCode == http.StatusBadRequest {
					bodyBytes, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if m := reXSRF.FindSubmatch(bodyBytes); len(m) > 1 {
						freshXSRF := string(m[1])
						c.Logf("[xsrf] Upstream 400 provided fresh XSRF token, retrying...")
						cfg.XSRFToken = freshXSRF
						bodyStr = BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg, opt.IsVideo)
						attempt-- // retry with fresh xsrf
						continue
					}
				} else {
					_ = resp.Body.Close()
				}

				if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusForbidden {
					c.Cookies.MarkFailed(cookieInfo.Cookie, 15*time.Minute)
					if c.Cookies.TotalAccounts() > 1 && attempt < maxAttempts-1 {
						cookieInfo, _ = c.Cookies.Load()
						cfg.XSRFToken = cookieInfo.XSRFToken
						cfg.AuthUser = cookieInfo.AuthUser
						bodyStr = BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg, opt.IsVideo)
						headers = c.buildHeaders()
						if opt.IsVideo {
							headers.Set(ModelHeaderKey, BuildVideoHeader(reqUUID, c.CurrentBL()))
							headers.Set(HeaderJspb89, "[0]")
							headers.Set(HeaderJspb90, "[0,0,0]")
						} else if modelHdr != "" {
							headers.Set(ModelHeaderKey, modelHdr)
							headers.Set(HeaderJspb89, "[0]")
							headers.Set(HeaderJspb90, "[0,0,0]")
						}
						continue
					}
				}
				// 405 = BL expired. Refresh once and retry without consuming an attempt.
				if !blRefreshed && resp.StatusCode == http.StatusMethodNotAllowed {
					blRefreshed = true
					if _, changed, blErr := c.UpdateBLIfNeeded(); blErr == nil && changed {
						c.Logf("BL auto-updated, retrying request")
						bodyStr = BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg, opt.IsVideo)
						reqURL = BuildURL(cfg, c.CurrentBL())
						if opt.IsVideo {
							headers.Set(ModelHeaderKey, BuildVideoHeader(reqUUID, c.CurrentBL()))
						}
						attempt-- // retry without consuming an attempt
						continue
					}
				}
				lastErr = err
			} else {
				rawBytes, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					lastErr = &UpstreamError{Kind: "transport", Msg: err.Error()}
				} else {
					parsed := ExtractResponse(string(rawBytes))
					if parsed.Text == "" {
						if code, ok := IsBardError(string(rawBytes)); ok {
							lastErr = &UpstreamError{Kind: "bard", Msg: fmt.Sprintf("gemini upstream rejected request: BardErrorInfo [%s]", code)}
						} else {
							lastErr = &UpstreamError{Kind: "bard", Msg: "empty response from upstream"}
						}
					} else {
						textLower := strings.ToLower(parsed.Text)
						isQuotaOrRefusal := false
						if opt.IsVideo {
							if strings.Contains(textLower, "usage limit") ||
								strings.Contains(textLower, "limit resets") ||
								strings.Contains(textLower, "can't create it right now") ||
								strings.Contains(textLower, "can't seem to create") ||
								strings.Contains(textLower, "are you signed in") ||
								strings.Contains(textLower, "create more videos as soon as") ||
								strings.Contains(textLower, "search for images, but can't create") ||
								strings.Contains(textLower, "search for images, but can't seem") ||
								strings.Contains(textLower, "tidak dapat membuat") {
								isQuotaOrRefusal = true
							}
						} else {
							if strings.Contains(textLower, "getting a lot of requests") ||
								strings.Contains(textLower, "usage limit reached") ||
								strings.Contains(textLower, "limit resets") ||
								strings.Contains(textLower, "can't create it right now") {
								isQuotaOrRefusal = true
							}
						}

						if isQuotaOrRefusal {
							c.Logf("[cookie-pool] Quota refusal detected (text=%q)", parsed.Text[:min(len(parsed.Text), 120)])
							c.Cookies.MarkFailed(cookieInfo.Cookie, 15*time.Minute)
							if c.Cookies.TotalAccounts() > 1 && attempt < maxAttempts-1 {
								c.Logf("[cookie-pool] Account quota/feature limit reached, falling over to next account (%d/%d)...", attempt+1, maxAttempts)
								cookieInfo, _ = c.Cookies.Load()
								cfg.XSRFToken = cookieInfo.XSRFToken
								cfg.AuthUser = cookieInfo.AuthUser
								bodyStr = BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg, opt.IsVideo)
								headers = c.buildHeaders()
								if opt.IsVideo {
									headers.Set(ModelHeaderKey, BuildVideoHeader(reqUUID, c.CurrentBL()))
									headers.Set(HeaderJspb89, "[0]")
									headers.Set(HeaderJspb90, "[0,0,0]")
								} else if modelHdr != "" {
									headers.Set(ModelHeaderKey, modelHdr)
									headers.Set(HeaderJspb89, "[0]")
									headers.Set(HeaderJspb90, "[0,0,0]")
								}
								continue
							}
						}
						return GenerateResult{Text: parsed.Text, ConversationID: parsed.ConversationID}, nil
					}
				}
			}
		}

		if attempt < maxAttempts-1 {
			c.Logf("Retry %d/%d: %v", attempt+1, maxAttempts, lastErr)
			time.Sleep(time.Duration(c.Cfg.RetryDelaySec) * time.Second)
		}
	}

	return GenerateResult{}, lastErr
}

func (c *Client) GenerateStream(prompt string, opt ModelOptions, fileRefs []string, emit func(string) error) error {
	c.EnsureModelsDiscovered()

	cookieInfo, _ := c.Cookies.Load()
	cfg := c.Cfg
	if cfg.XSRFToken == "" && cookieInfo.XSRFToken != "" {
		cfg.XSRFToken = cookieInfo.XSRFToken
	}
	if cfg.AuthUser == "" && cookieInfo.AuthUser != "" {
		cfg.AuthUser = cookieInfo.AuthUser
	}

	reqUUID := strings.ToUpper(uuidV4())
	extra := make(map[int]any, len(opt.Extra)+1)
	for k, v := range opt.Extra {
		extra[k] = v
	}
	extra[59] = reqUUID

	modelHdr, effectiveMode := c.resolveModelHeader(opt, reqUUID)
	bodyStr := BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg, opt.IsVideo)
	reqURL := BuildURL(cfg, c.CurrentBL())
	headers := c.buildHeaders()
	headers.Set(HeaderUUIDJspb, fmt.Sprintf(`["%s",1]`, reqUUID))
	if opt.IsVideo {
		headers.Set(ModelHeaderKey, BuildVideoHeader(reqUUID, c.CurrentBL()))
		headers.Set(HeaderJspb89, "[0]")
		headers.Set(HeaderJspb90, "[0,0,0]")
	} else if modelHdr != "" {
		headers.Set(ModelHeaderKey, modelHdr)
		headers.Set(HeaderJspb89, "[0]")
		headers.Set(HeaderJspb90, "[0,0,0]")
	}

	blRefreshed := false
	parser := NewStreamParser()
	var lastErr error

	for attempt := 0; attempt < c.Cfg.RetryAttempts; attempt++ {
		parser.ResetBuffer()

		req, err := http.NewRequest("POST", reqURL, strings.NewReader(bodyStr))
		if err != nil {
			return err
		}
		req.Header = headers.Clone()

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = &UpstreamError{Kind: "transport", Msg: err.Error()}
		} else {
			if err := c.triageStatus(resp); err != nil {
				_ = resp.Body.Close()
				// 405 = BL expired. Refresh once and retry without consuming an attempt.
				if !blRefreshed && resp.StatusCode == http.StatusMethodNotAllowed {
					blRefreshed = true
					if _, changed, blErr := c.UpdateBLIfNeeded(); blErr == nil && changed {
						c.Logf("BL auto-updated, retrying stream request")
						bodyStr = BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg, opt.IsVideo)
						reqURL = BuildURL(cfg, c.CurrentBL())
						if opt.IsVideo {
							headers.Set(ModelHeaderKey, BuildVideoHeader(reqUUID, c.CurrentBL()))
						}
						attempt-- // retry without consuming an attempt
						continue
					}
				}
				lastErr = err
			} else {
				err = c.streamAttempt(resp.Body, parser, emit)
				_ = resp.Body.Close()
				if err == nil {
					return nil
				}
				if isClientDisconnect(err) {
					return err
				}
				lastErr = err
			}
		}

		if attempt < c.Cfg.RetryAttempts-1 {
			c.Logf("Stream retry %d/%d: %v", attempt+1, c.Cfg.RetryAttempts, lastErr)
			time.Sleep(time.Duration(c.Cfg.RetryDelaySec) * time.Second)
		}
	}

	return lastErr
}

func (c *Client) streamAttempt(body io.Reader, parser *StreamParser, emit func(string) error) error {
	reader := bufio.NewReader(body)
	for {
		lineBytes, err := reader.ReadBytes('\n')
		if len(lineBytes) > 0 {
			deltas, parseErr := parser.Feed(string(lineBytes))
			if parseErr != nil {
				return &UpstreamError{Kind: "bard", Msg: parseErr.Error()}
			}
			for _, delta := range deltas {
				if err := emit(delta); err != nil {
					return err
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return &UpstreamError{Kind: "transport", Msg: err.Error()}
		}
	}
}

// isClientDisconnect helps distinguish between upstream server errors (which we might want to retry)
// and actual client-side disconnections or non-retryable transport errors.
// If the client disconnected, we should abort the retry loop immediately to save resources.
func isClientDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*UpstreamError); ok {
		return false
	}
	return true
}
