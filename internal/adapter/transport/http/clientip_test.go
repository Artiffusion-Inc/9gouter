package http

import (
	"net/http/httptest"
	"testing"
)

func TestFromRequest(t *testing.T) {
	cases := []struct {
		name      string
		remote    string
		xff       string
		xRealIP   string
		want      string
	}{
		{
			name:   "direct socket ignores spoofed XFF",
			remote: "1.2.3.4:56789",
			xff:    "5.6.7.8",
			want:   "1.2.3.4",
		},
		{
			name:   "loopback trusts XFF",
			remote: "127.0.0.1:56789",
			xff:    "5.6.7.8",
			want:   "5.6.7.8",
		},
		{
			name:    "loopback trusts X-Real-IP over XFF",
			remote:  "127.0.0.1:56789",
			xff:     "5.6.7.8",
			xRealIP: "9.10.11.12",
			want:    "9.10.11.12",
		},
		{
			name:   "loopback no headers uses socket IP",
			remote: "127.0.0.1:56789",
			want:   "127.0.0.1",
		},
		{
			name:   "IPv6 loopback trusts XFF",
			remote: "[::1]:56789",
			xff:    "5.6.7.8",
			want:   "5.6.7.8",
		},
		{
			name:    "IPv4-mapped loopback trusts X-Real-IP",
			remote:  "[::ffff:127.0.0.1]:56789",
			xRealIP: "5.6.7.8",
			want:    "5.6.7.8",
		},
		{
			name:   "direct socket with X-Real-IP ignored",
			remote: "1.2.3.4:56789",
			xRealIP: "5.6.7.8",
			want:   "1.2.3.4",
		},
		{
			name:   "XFF with multiple entries uses first",
			remote: "127.0.0.1:56789",
			xff:    "5.6.7.8, 9.10.11.12, 13.14.15.16",
			want:   "5.6.7.8",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xRealIP != "" {
				r.Header.Set("X-Real-Ip", tc.xRealIP)
			}
			got := FromRequest(r)
			if got != tc.want {
				t.Errorf("FromRequest() = %q, want %q", got, tc.want)
			}
		})
	}
}
