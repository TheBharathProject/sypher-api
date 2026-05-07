package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleUserInfoURL = "https://www.googleapis.com/oauth2/v3/userinfo"
)

// BuildGoogleAuthURL constructs the URL to redirect the user to Google's
// consent screen. State is a random opaque string we set in a short-lived
// cookie so we can validate the callback came from us.
func BuildGoogleAuthURL(clientID, redirectURI, state string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	q.Set("prompt", "select_account")
	q.Set("access_type", "online")
	return googleAuthURL + "?" + q.Encode()
}

// RandomState returns a URL-safe random string usable as OAuth state.
func RandomState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ExchangeCode swaps an OAuth authorization code for the user's Google
// profile. Two HTTP round-trips: token exchange, then userinfo.
func ExchangeCode(ctx context.Context, clientID, clientSecret, redirectURI, code string) (*GoogleProfile, error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")

	client := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token req: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange status %d: %s", resp.StatusCode, string(body))
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("empty access token")
	}

	uReq, err := http.NewRequestWithContext(ctx, http.MethodGet, googleUserInfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build userinfo req: %w", err)
	}
	uReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uResp, err := client.Do(uReq)
	if err != nil {
		return nil, fmt.Errorf("userinfo: %w", err)
	}
	defer uResp.Body.Close()
	uBody, _ := io.ReadAll(io.LimitReader(uResp.Body, 64*1024))
	if uResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo status %d: %s", uResp.StatusCode, string(uBody))
	}

	var prof GoogleProfile
	if err := json.Unmarshal(uBody, &prof); err != nil {
		return nil, fmt.Errorf("decode userinfo: %w", err)
	}
	if prof.Sub == "" || prof.Email == "" {
		return nil, fmt.Errorf("userinfo missing sub/email")
	}
	return &prof, nil
}
