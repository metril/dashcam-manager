package auth

import (
	"slices"
	"strings"

	"dashcam/internal/config"
)

// Authorize decides whether the identity in claims may use the app and
// returns its display email and name. With both allow-lists empty any
// authenticated user is allowed; otherwise the user needs a verified email
// on AllowedEmails (case-insensitive) or a group on AllowedGroups.
func Authorize(cfg config.OIDCConfig, claims map[string]any) (email, name string, ok bool) {
	email, _ = claims["email"].(string)
	name = firstString(claims, "name", "preferred_username")
	if name == "" {
		name = email
	}

	if len(cfg.AllowedEmails) == 0 && len(cfg.AllowedGroups) == 0 {
		return email, name, true
	}
	verified := true // absent claim is treated as verified; only an explicit false rejects
	if v, present := claims["email_verified"].(bool); present && !v {
		verified = false
	}
	if verified && email != "" && slices.Contains(cfg.AllowedEmails, strings.ToLower(email)) {
		return email, name, true
	}
	for _, g := range Groups(cfg, claims) {
		if slices.Contains(cfg.AllowedGroups, g) {
			return email, name, true
		}
	}
	return email, name, false
}

// Groups extracts the groups claim, which IdPs send as an array or (some)
// as a single string.
func Groups(cfg config.OIDCConfig, claims map[string]any) []string {
	switch v := claims[cfg.GroupsClaim].(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func firstString(claims map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, _ := claims[k].(string); s != "" {
			return s
		}
	}
	return ""
}
