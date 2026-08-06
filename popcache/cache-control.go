package main

import (
	"strconv"
	"strings"
)

type RequestCacheControl struct {
	MaxAge  int64
	NoCache bool
	NoStore bool
}

type ResponseCacheControl struct {
	MaxAge int64

	MustRevalidate bool
	NoCache        bool
	NoStore        bool

	StaleWhileRevalidate int64
	StaleIfError         int64
}

type cacheDirective struct {
	value     string
	hasValue  bool
	invalid   bool
	duplicate bool
}

type cacheDirectives map[string]cacheDirective

func splitCacheControl(raw string) []string {
	var result []string

	start := 0
	quoted := false
	escaped := false

	for i := 0; i < len(raw); i++ {
		ch := raw[i]

		if escaped {
			escaped = false
			continue
		}

		if quoted && ch == '\\' {
			escaped = true
			continue
		}

		if ch == '"' {
			quoted = !quoted
			continue
		}

		if ch == ',' && !quoted {
			result = append(result, raw[start:i])
			start = i + 1
		}
	}

	result = append(result, raw[start:])
	return result
}

func parseCacheControl(values []string) cacheDirectives {
	raw := strings.Join(values, ",")
	result := make(cacheDirectives)

	for _, part := range splitCacheControl(raw) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		name, value, hasValue := strings.Cut(part, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}

		directive := cacheDirective{
			hasValue: hasValue,
		}

		if old, exists := result[name]; exists {
			old.duplicate = true
			result[name] = old
			continue
		}

		if hasValue {
			value = strings.TrimSpace(value)

			if len(value) >= 2 &&
				value[0] == '"' &&
				value[len(value)-1] == '"' {

				value = value[1 : len(value)-1]
			} else if strings.HasPrefix(value, `"`) ||
				strings.HasSuffix(value, `"`) {

				directive.invalid = true
			}

			directive.value = value
		}

		result[name] = directive
	}

	return result
}

func hasDirective(mp cacheDirectives, key string) bool {
	_, ok := mp[key]
	return ok
}

func deltaSeconds(mp cacheDirectives, key string) int64 {
	directive, ok := mp[key]
	if !ok {
		return -1
	}

	if !directive.hasValue || directive.invalid || directive.duplicate {
		return 0
	}

	value, err := strconv.ParseInt(directive.value, 10, 64)
	if err != nil || value < 0 {
		return 0
	}

	return value
}

func ParseRequestCacheControl(values []string) RequestCacheControl {
	mp := parseCacheControl(values)

	return RequestCacheControl{
		MaxAge:  deltaSeconds(mp, "max-age"),
		NoCache: hasDirective(mp, "no-cache"),
		NoStore: hasDirective(mp, "no-store"),
	}
}

func ParseResponseCacheControl(values []string) ResponseCacheControl {
	mp := parseCacheControl(values)

	maxAge := deltaSeconds(mp, "max-age")
	sMaxAge := deltaSeconds(mp, "s-maxage")

	if hasDirective(mp, "s-maxage") {
		maxAge = sMaxAge
	}

	return ResponseCacheControl{
		MaxAge: maxAge,

		MustRevalidate: hasDirective(mp, "must-revalidate") ||
			hasDirective(mp, "proxy-revalidate") ||
			hasDirective(mp, "s-maxage"),

		NoCache: hasDirective(mp, "no-cache"),

		NoStore: hasDirective(mp, "no-store") || hasDirective(mp, "private"),

		StaleWhileRevalidate: deltaSeconds(mp, "stale-while-revalidate"),
		StaleIfError:         deltaSeconds(mp, "stale-if-error"),
	}
}
