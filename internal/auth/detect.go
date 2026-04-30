// Package auth holds small heuristics shared across packages that need to
// reason about authentication state — currently snapshot perception (does
// this look like a login wall?) and credential capture (is the user past
// the login flow yet?).
package auth

import "strings"

// IsLoginURL returns true for URLs we believe represent an unfinished auth
// flow (login form, OAuth handoff, MFA / checkpoint, blank tabs, etc).
//
// We err on the side of "not yet logged in" — false positives just delay
// downstream behaviour (e.g. credential capture); false negatives can produce
// useless captures or miss auth-wall signals to agents.
func IsLoginURL(u string) bool {
	if u == "" {
		return true
	}
	lu := strings.ToLower(u)
	if strings.HasPrefix(lu, "about:") ||
		strings.HasPrefix(lu, "chrome:") ||
		strings.HasPrefix(lu, "data:") ||
		strings.HasPrefix(lu, "file:") {
		return true
	}
	patterns := []string{
		"/login", "/signin", "/sign-in", "/sign_in", "/signup", "/sign-up",
		"/auth/", "/oauth/", "/openid/", "/sso/", "/uas/",
		"/checkpoint/", "/challenge/", "/verify", "/verification",
		"/2fa", "/otp", "/mfa",
		"accounts.google.com", // Google's login lives on a separate origin
	}
	for _, p := range patterns {
		if strings.Contains(lu, p) {
			return true
		}
	}
	return false
}
