package jobtracker

import (
	"fmt"
	"net/mail"
	"net/url"
	"strings"
)

// ValidateURL accepts only http/https URLs with a non-empty host.
// Empty input is treated as "field not provided" and passes — every
// caller is for optional URL fields. Returns a nil error on success;
// the error message is safe to show to users (no internal details).
//
// Centralises the Naukri Clear §8.4 fix: their LinkedIn field accepted
// any string ("hasdkjaamc"), corrupting the public profile renderer.
// Apply this in handlers anywhere a URL field arrives from the client.
func ValidateURL(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	u, err := url.ParseRequestURI(s)
	if err != nil {
		return fmt.Errorf("not a valid URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("URL must start with http:// or https://")
	}
	if u.Host == "" {
		return fmt.Errorf("URL is missing a host")
	}
	return nil
}

// parseEmail validates an email address using net/mail. Returns the parsed
// address on success; returns an error when the input is not a valid address.
func parseEmail(s string) (*mail.Address, error) {
	return mail.ParseAddress(s)
}
