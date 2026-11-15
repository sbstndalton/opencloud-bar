package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

type quotaInfo struct {
	Total     int64  `json:"total"`
	Used      int64  `json:"used"`
	Remaining int64  `json:"remaining"`
	State     string `json:"state"` // normal, nearing, critical, exceeded
}

// accountInfo is what the Account page shows. It holds no tokens.
type accountInfo struct {
	Mode         string     `json:"mode"` // "oidc", "legacy" or "none"
	Server       string     `json:"server,omitempty"`
	Username     string     `json:"username,omitempty"`
	DisplayName  string     `json:"displayName,omitempty"`
	Email        string     `json:"email,omitempty"`
	Quota        *quotaInfo `json:"quota,omitempty"`
	Version      string     `json:"version,omitempty"`
	BlockedNames []string   `json:"blockedNames"`
	Undelete     *bool      `json:"undelete,omitempty"`
	Versioning   *bool      `json:"versioning,omitempty"`
	SignedOut    bool       `json:"signedOut,omitempty"`
	Note         string     `json:"note,omitempty"`
	Errors       []string   `json:"errors"` // one line per call that failed; the rest still shows
}

// davUser is the account name in a davPath like /remote.php/dav/files/NAME.
func davUser(davPath string) string {
	n := path.Base(strings.TrimRight(davPath, "/"))
	if u, err := url.PathUnescape(n); err == nil {
		n = u
	}
	if n == "." || n == "/" || n == "USER" {
		return ""
	}
	return n
}

func (e *Engine) uiAccount(ctx context.Context) accountInfo {
	cfg := e.config()
	st := e.Status()
	info := accountInfo{BlockedNames: []string{}, Errors: []string{}, Server: cfg.ServerURL, Username: davUser(cfg.DavPath)}
	switch {
	case !cfg.Configured():
		info.Mode = "none"
		info.Note = "Not set up yet."
		return info
	case !cfg.OIDC():
		info.Mode = "legacy"
		info.Note = "Signed in with an app token (advanced)."
		if cfg.Tunnel() && info.Server == "" {
			info.Server = "through the SSH tunnel to " + cfg.SSHHost
		}
		return info
	}
	info.Mode = "oidc"
	if st.SignedOut {
		info.SignedOut = true
		info.Note = "Signed out."
	}
	tok := ""
	if !st.SignedOut {
		t, err := accessToken(cfg.ServerURL)
		if err != nil {
			info.Errors = append(info.Errors, "couldn't get a sign-in: "+err.Error())
		} else {
			tok = t
		}
	}
	fetchAccount(ctx, cfg.ServerURL, tok, &info)
	return info
}

var accountClient = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// ocGet fetches JSON from the server. tok may be empty (the capabilities need no sign-in).
// Errors never contain the token or the response body.
func ocGet(ctx context.Context, base, p string, q url.Values, tok string, out any) error {
	u := base + p
	if q != nil {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := accountClient.Do(req)
	if err != nil {
		return errors.New("the server didn't answer")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out); err != nil {
		return errors.New("the server's answer wasn't what was expected")
	}
	return nil
}

// fetchAccount fills info from the OpenCloud OCS capabilities and Graph APIs.
func fetchAccount(ctx context.Context, serverURL, tok string, info *accountInfo) {
	base, err := normalizeServerURL(serverURL)
	if err != nil {
		info.Errors = append(info.Errors, err.Error())
		return
	}
	info.Server = base

	var caps struct {
		OCS struct {
			Data struct {
				Version struct {
					String string `json:"string"`
				} `json:"version"`
				Capabilities struct {
					Files struct {
						Blacklisted []string `json:"blacklisted_files"`
						Undelete    *bool    `json:"undelete"`
						Versioning  *bool    `json:"versioning"`
					} `json:"files"`
				} `json:"capabilities"`
			} `json:"data"`
		} `json:"ocs"`
	}
	if err := ocGet(ctx, base, "/ocs/v1.php/cloud/capabilities", url.Values{"format": {"json"}}, "", &caps); err != nil {
		info.Errors = append(info.Errors, "server details: "+err.Error())
	} else {
		d := caps.OCS.Data
		info.Version = d.Version.String
		info.BlockedNames = append(info.BlockedNames, d.Capabilities.Files.Blacklisted...)
		info.Undelete, info.Versioning = d.Capabilities.Files.Undelete, d.Capabilities.Files.Versioning
	}
	if tok == "" {
		return
	}
	var me struct {
		DisplayName string `json:"displayName"`
		Mail        string `json:"mail"`
		SamAccount  string `json:"onPremisesSamAccountName"`
	}
	if err := ocGet(ctx, base, "/graph/v1.0/me", nil, tok, &me); err != nil {
		info.Errors = append(info.Errors, "account: "+err.Error())
	} else {
		info.DisplayName, info.Email = me.DisplayName, me.Mail
		if me.SamAccount != "" {
			info.Username = me.SamAccount
		}
	}
	var drives struct {
		Value []struct {
			Quota *quotaInfo `json:"quota"`
		} `json:"value"`
	}
	if err := ocGet(ctx, base, "/graph/v1.0/me/drives", url.Values{"$filter": {"driveType eq 'personal'"}}, tok, &drives); err != nil {
		info.Errors = append(info.Errors, "storage: "+err.Error())
	} else if len(drives.Value) > 0 && drives.Value[0].Quota != nil {
		info.Quota = drives.Value[0].Quota
	}
}
