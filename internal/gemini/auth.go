package gemini

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

type CookieInfo struct {
	Cookie    string
	SAPISID   string
	AuthUser  string
	XSRFToken string
	GeminiBL  string
}

type accountEntry struct {
	info        CookieInfo
	failedUntil time.Time
}

type CookieCache struct {
	mu        sync.Mutex
	file      string
	mtime     time.Time
	accounts  []accountEntry
	activeIdx int
}

func NewCookieCache(file string) *CookieCache {
	return &CookieCache{file: file}
}

func parseSingleCookie(content string) CookieInfo {
	content = strings.TrimSpace(content)
	if content == "" {
		return CookieInfo{}
	}

	var cookieStr string
	var sapisid string
	var authUser string
	var xsrfToken string
	var geminiBL string

	if strings.HasPrefix(content, "{") {
		var data struct {
			Cookie    string          `json:"cookie"`
			SAPISID   string          `json:"sapisid"`
			AuthUser  json.RawMessage `json:"auth_user"`
			XSRFToken *string         `json:"xsrf_token"`
			GeminiBL  *string         `json:"gemini_bl"`
		}
		if err := json.Unmarshal([]byte(content), &data); err == nil {
			cookieStr = data.Cookie
			sapisid = data.SAPISID
			if len(data.AuthUser) > 0 {
				u := strings.Trim(strings.TrimSpace(string(data.AuthUser)), `"`)
				if u != "null" {
					authUser = u
				}
			}
			if data.XSRFToken != nil {
				xsrfToken = *data.XSRFToken
			}
			if data.GeminiBL != nil {
				geminiBL = *data.GeminiBL
			}
		}
	} else {
		cookieStr = content
	}

	if sapisid == "" && cookieStr != "" {
		pairs := strings.Split(cookieStr, "; ")
		for _, pair := range pairs {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 && parts[0] == "SAPISID" {
				sapisid = parts[1]
				break
			}
		}
	}

	return CookieInfo{
		Cookie:    cookieStr,
		SAPISID:   sapisid,
		AuthUser:  authUser,
		XSRFToken: xsrfToken,
		GeminiBL:  geminiBL,
	}
}

func (c *CookieCache) loadFromFileLocked() error {
	stat, err := os.Stat(c.file)
	if err != nil {
		if len(c.accounts) > 0 {
			return nil
		}
		return err
	}

	if stat.ModTime().Equal(c.mtime) && len(c.accounts) > 0 {
		return nil
	}

	contentBytes, err := os.ReadFile(c.file)
	if err != nil {
		if len(c.accounts) > 0 {
			return nil
		}
		return err
	}

	content := strings.TrimSpace(string(contentBytes))
	var newInfos []CookieInfo

	if strings.HasPrefix(content, "[") {
		// JSON array of strings or objects
		var rawList []json.RawMessage
		if err := json.Unmarshal([]byte(content), &rawList); err == nil {
			for _, item := range rawList {
				s := string(item)
				if strings.HasPrefix(s, `"`) {
					var str string
					if err := json.Unmarshal(item, &str); err == nil && str != "" {
						newInfos = append(newInfos, parseSingleCookie(str))
					}
				} else {
					newInfos = append(newInfos, parseSingleCookie(s))
				}
			}
		}
	} else {
		// Multi-line plain text or single string
		lines := strings.Split(content, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			info := parseSingleCookie(line)
			if info.Cookie != "" {
				newInfos = append(newInfos, info)
			}
		}
	}

	if len(newInfos) == 0 && content != "" {
		info := parseSingleCookie(content)
		if info.Cookie != "" {
			newInfos = append(newInfos, info)
		}
	}

	c.mtime = stat.ModTime()

	// Merge cooldowns if accounts already existed
	existingCooldowns := make(map[string]time.Time)
	for _, acc := range c.accounts {
		existingCooldowns[acc.info.Cookie] = acc.failedUntil
	}

	c.accounts = make([]accountEntry, len(newInfos))
	for i, info := range newInfos {
		c.accounts[i] = accountEntry{
			info:        info,
			failedUntil: existingCooldowns[info.Cookie],
		}
	}

	if c.activeIdx >= len(c.accounts) {
		c.activeIdx = 0
	}

	log.Printf("[cookie-pool] Loaded %d account(s) from %s (active: account %d)", len(c.accounts), c.file, c.activeIdx+1)
	return nil
}

// Load returns the current active account. Uses sequential multistep failover:
// Account 1 is always used until it fails/cools down, then moves to Account 2, etc.
func (c *CookieCache) Load() (CookieInfo, error) {
	if c.file == "" {
		return CookieInfo{}, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.loadFromFileLocked(); err != nil {
		return CookieInfo{}, err
	}

	if len(c.accounts) == 0 {
		return CookieInfo{}, nil
	}

	now := time.Now()

	// If current active account is still on cooldown, find the next available account
	if now.Before(c.accounts[c.activeIdx].failedUntil) {
		found := false
		for step := 1; step <= len(c.accounts); step++ {
			next := (c.activeIdx + step) % len(c.accounts)
			if now.After(c.accounts[next].failedUntil) {
				c.activeIdx = next
				log.Printf("[cookie-pool] Current account in cooldown, switched to account %d of %d", next+1, len(c.accounts))
				found = true
				break
			}
		}
		if !found {
			log.Printf("[cookie-pool] All %d accounts are cooling down, using account %d as best-effort", len(c.accounts), c.activeIdx+1)
		}
	}

	return c.accounts[c.activeIdx].info, nil
}

// MarkFailed marks the given account cookie as failed and sets cooldown duration.
// It immediately advances activeIdx to the next available account in line (multistep).
func (c *CookieCache) MarkFailed(cookieStr string, cooldown time.Duration) {
	if cookieStr == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for i := range c.accounts {
		if c.accounts[i].info.Cookie == cookieStr {
			c.accounts[i].failedUntil = now.Add(cooldown)
			log.Printf("[cookie-pool] Account %d cooled down for %v (until %s)", i+1, cooldown, c.accounts[i].failedUntil.Format("15:04:05"))

			// If this was the active account, advance to next ready account
			if i == c.activeIdx {
				for step := 1; step <= len(c.accounts); step++ {
					next := (i + step) % len(c.accounts)
					if now.After(c.accounts[next].failedUntil) {
						c.activeIdx = next
						log.Printf("[cookie-pool] Multistep failover: switched active account to %d of %d", next+1, len(c.accounts))
						break
					}
				}
			}
			break
		}
	}
}

// TotalAccounts returns the count of loaded accounts.
func (c *CookieCache) TotalAccounts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.accounts)
}

func SAPISIDHash(sapisid string) string {
	if sapisid == "" {
		return ""
	}
	ts := time.Now().Unix()
	input := fmt.Sprintf("%d %s https://gemini.google.com", ts, sapisid)
	h := sha1.Sum([]byte(input))
	return fmt.Sprintf("SAPISIDHASH %d_%s", ts, hex.EncodeToString(h[:]))
}

func AccountPrefix(authUser string) string {
	if authUser == "" {
		return ""
	}
	return "/u/" + authUser
}
