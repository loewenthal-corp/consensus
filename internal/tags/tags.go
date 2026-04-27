package tags

import (
	"sort"
	"strings"
)

// Normalize returns the canonical form for a retrieval tag.
func Normalize(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	if key, rest, ok := strings.Cut(value, ":"); ok {
		key = strings.ToLower(strings.TrimSpace(key))
		rest = strings.TrimSpace(rest)
		if key == "" || rest == "" {
			return ""
		}
		return key + ":" + rest
	}
	if key, rest, ok := strings.Cut(value, "="); ok {
		key = strings.ToLower(strings.TrimSpace(key))
		rest = strings.TrimSpace(rest)
		if key == "" || rest == "" {
			return ""
		}
		return key + ":" + rest
	}
	return strings.ToLower(value)
}

func NormalizeList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = Normalize(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

func Parse(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	return NormalizeList(parts)
}
