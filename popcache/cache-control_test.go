// AI-generated test
package main

import (
	"reflect"
	"testing"
)

func TestSplitCacheControl(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "empty",
			raw:  "",
			want: []string{""},
		},
		{
			name: "without spaces",
			raw:  "max-age=60,no-cache,no-store",
			want: []string{"max-age=60", "no-cache", "no-store"},
		},
		{
			name: "comma inside quoted string",
			raw:  `no-cache="Set-Cookie, Authorization",max-age=60`,
			want: []string{
				`no-cache="Set-Cookie, Authorization"`,
				"max-age=60",
			},
		},
		{
			name: "escaped quote and comma",
			raw:  `example="foo\",bar",max-age=10`,
			want: []string{
				`example="foo\",bar"`,
				"max-age=10",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitCacheControl(tt.raw)

			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf(
					"splitCacheControl(%q)\ngot:  %#v\nwant: %#v",
					tt.raw,
					got,
					tt.want,
				)
			}
		})
	}
}

func TestParseRequestCacheControl(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   RequestCacheControl
	}{
		{
			name:   "empty",
			values: nil,
			want: RequestCacheControl{
				MaxAge: -1,
			},
		},
		{
			name:   "max age",
			values: []string{"max-age=60"},
			want: RequestCacheControl{
				MaxAge: 60,
			},
		},
		{
			name:   "all supported directives",
			values: []string{"max-age=60, no-cache, no-store"},
			want: RequestCacheControl{
				MaxAge:  60,
				NoCache: true,
				NoStore: true,
			},
		},
		{
			name: "multiple header fields",
			values: []string{
				"max-age=120",
				"no-cache",
				"no-store",
			},
			want: RequestCacheControl{
				MaxAge:  120,
				NoCache: true,
				NoStore: true,
			},
		},
		{
			name:   "case insensitive names",
			values: []string{`MAX-AGE="30", No-Cache, NO-STORE`},
			want: RequestCacheControl{
				MaxAge:  30,
				NoCache: true,
				NoStore: true,
			},
		},
		{
			name:   "explicit zero",
			values: []string{"max-age=0"},
			want: RequestCacheControl{
				MaxAge: 0,
			},
		},
		{
			name:   "qualified no cache treated as no cache",
			values: []string{`no-cache="Authorization"`},
			want: RequestCacheControl{
				MaxAge:  -1,
				NoCache: true,
			},
		},
		{
			name:   "invalid integer becomes zero",
			values: []string{"max-age=abc"},
			want: RequestCacheControl{
				MaxAge: 0,
			},
		},
		{
			name:   "negative integer becomes zero",
			values: []string{"max-age=-1"},
			want: RequestCacheControl{
				MaxAge: 0,
			},
		},
		{
			name:   "missing integer becomes zero",
			values: []string{"max-age"},
			want: RequestCacheControl{
				MaxAge: 0,
			},
		},
		{
			name:   "malformed quoted integer becomes zero",
			values: []string{`max-age="60`},
			want: RequestCacheControl{
				MaxAge: 0,
			},
		},
		{
			name:   "duplicate max age becomes zero",
			values: []string{"max-age=60, max-age=120"},
			want: RequestCacheControl{
				MaxAge: 0,
			},
		},
		{
			name:   "unknown directives ignored",
			values: []string{"foo=123, bar, max-age=10"},
			want: RequestCacheControl{
				MaxAge: 10,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseRequestCacheControl(tt.values)

			if got != tt.want {
				t.Fatalf(
					"ParseRequestCacheControl(%q)\ngot:  %+v\nwant: %+v",
					tt.values,
					got,
					tt.want,
				)
			}
		})
	}
}

func TestParseResponseCacheControl(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   ResponseCacheControl
	}{
		{
			name:   "empty",
			values: nil,
			want: ResponseCacheControl{
				MaxAge:               -1,
				StaleWhileRevalidate: -1,
				StaleIfError:         -1,
			},
		},
		{
			name: "all supported directives",
			values: []string{
				"max-age=60, must-revalidate, no-cache",
				"stale-while-revalidate=30, stale-if-error=300",
			},
			want: ResponseCacheControl{
				MaxAge:               60,
				MustRevalidate:       true,
				NoCache:              true,
				StaleWhileRevalidate: 30,
				StaleIfError:         300,
			},
		},
		{
			name:   "s maxage overrides max age",
			values: []string{"max-age=60, s-maxage=120"},
			want: ResponseCacheControl{
				MaxAge:               120,
				MustRevalidate:       true,
				StaleWhileRevalidate: -1,
				StaleIfError:         -1,
			},
		},
		{
			name:   "s maxage alone",
			values: []string{"s-maxage=300"},
			want: ResponseCacheControl{
				MaxAge:               300,
				MustRevalidate:       true,
				StaleWhileRevalidate: -1,
				StaleIfError:         -1,
			},
		},
		{
			name:   "proxy revalidate",
			values: []string{"max-age=60, proxy-revalidate"},
			want: ResponseCacheControl{
				MaxAge:               60,
				MustRevalidate:       true,
				StaleWhileRevalidate: -1,
				StaleIfError:         -1,
			},
		},
		{
			name:   "private treated as no store",
			values: []string{"max-age=60, private"},
			want: ResponseCacheControl{
				MaxAge:               60,
				NoStore:              true,
				StaleWhileRevalidate: -1,
				StaleIfError:         -1,
			},
		},
		{
			name:   "qualified private treated as no store",
			values: []string{`private="Set-Cookie", max-age=60`},
			want: ResponseCacheControl{
				MaxAge:               60,
				NoStore:              true,
				StaleWhileRevalidate: -1,
				StaleIfError:         -1,
			},
		},
		{
			name:   "no store",
			values: []string{"no-store"},
			want: ResponseCacheControl{
				MaxAge:               -1,
				NoStore:              true,
				StaleWhileRevalidate: -1,
				StaleIfError:         -1,
			},
		},
		{
			name:   "quoted delta seconds",
			values: []string{`max-age="60", stale-if-error="300"`},
			want: ResponseCacheControl{
				MaxAge:               60,
				StaleWhileRevalidate: -1,
				StaleIfError:         300,
			},
		},
		{
			name:   "case insensitive",
			values: []string{"S-MAXAGE=100, STALE-WHILE-REVALIDATE=20"},
			want: ResponseCacheControl{
				MaxAge:               100,
				MustRevalidate:       true,
				StaleWhileRevalidate: 20,
				StaleIfError:         -1,
			},
		},
		{
			name:   "duplicate max age becomes zero",
			values: []string{"max-age=60, max-age=120"},
			want: ResponseCacheControl{
				MaxAge:               0,
				StaleWhileRevalidate: -1,
				StaleIfError:         -1,
			},
		},
		{
			name:   "duplicate s maxage overrides max age with zero",
			values: []string{"max-age=60, s-maxage=100, s-maxage=200"},
			want: ResponseCacheControl{
				MaxAge:               0,
				MustRevalidate:       true,
				StaleWhileRevalidate: -1,
				StaleIfError:         -1,
			},
		},
		{
			name:   "invalid stale durations become zero",
			values: []string{"max-age=60, stale-while-revalidate=x, stale-if-error=-1"},
			want: ResponseCacheControl{
				MaxAge:               60,
				StaleWhileRevalidate: 0,
				StaleIfError:         0,
			},
		},
		{
			name: "quoted comma does not split directive",
			values: []string{
				`no-cache="Set-Cookie, Authorization", max-age=60`,
			},
			want: ResponseCacheControl{
				MaxAge:               60,
				NoCache:              true,
				StaleWhileRevalidate: -1,
				StaleIfError:         -1,
			},
		},
		{
			name:   "unknown directives ignored",
			values: []string{"public, immutable, foo=bar, max-age=45"},
			want: ResponseCacheControl{
				MaxAge:               45,
				StaleWhileRevalidate: -1,
				StaleIfError:         -1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseResponseCacheControl(tt.values)

			if got != tt.want {
				t.Fatalf(
					"ParseResponseCacheControl(%q)\ngot:  %+v\nwant: %+v",
					tt.values,
					got,
					tt.want,
				)
			}
		})
	}
}

func TestParseCacheControlDuplicateMetadata(t *testing.T) {
	got := parseCacheControl([]string{
		"max-age=10, no-cache",
		"max-age=20",
	})

	maxAge, ok := got["max-age"]
	if !ok {
		t.Fatal("max-age directive not found")
	}
	if !maxAge.duplicate {
		t.Error("max-age duplicate=false, want true")
	}
	if maxAge.value != "10" {
		t.Errorf("max-age value=%q, want first value %q", maxAge.value, "10")
	}

	noCache, ok := got["no-cache"]
	if !ok {
		t.Fatal("no-cache directive not found")
	}
	if noCache.hasValue {
		t.Error("no-cache hasValue=true, want false")
	}
}

func FuzzParseCacheControlNeverPanics(f *testing.F) {
	seeds := []string{
		"",
		"max-age=60",
		"max-age=60,no-cache",
		`max-age="60", private`,
		`no-cache="Set-Cookie, Authorization"`,
		`foo="bar\",baz", s-maxage=10`,
		`max-age="`,
		",,,,,",
	}

	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		values := []string{raw}

		_ = parseCacheControl(values)
		_ = ParseRequestCacheControl(values)
		_ = ParseResponseCacheControl(values)
	})
}
