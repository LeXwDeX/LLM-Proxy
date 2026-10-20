package proxy

import (
	"net/http"
	"testing"
)

func TestIsUpstreamFailureStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   bool
	}{
		// 5xx：通用故障
		{"500 internal server error", http.StatusInternalServerError, true},
		{"502 bad gateway", http.StatusBadGateway, true},
		{"503 service unavailable", http.StatusServiceUnavailable, true},
		{"504 gateway timeout", http.StatusGatewayTimeout, true},
		{"599 unknown 5xx", 599, true},

		// 429/408：上游过载与超时，应触发 fallback
		{"408 request timeout", http.StatusRequestTimeout, true},
		{"429 too many requests", http.StatusTooManyRequests, true},

		// 其他 4xx：客户端问题，不切换 target
		{"400 bad request", http.StatusBadRequest, false},
		{"401 unauthorized", http.StatusUnauthorized, false},
		{"403 forbidden", http.StatusForbidden, false},
		{"404 not found", http.StatusNotFound, false},
		{"422 unprocessable entity", http.StatusUnprocessableEntity, false},

		// 2xx/3xx：正常响应
		{"200 ok", http.StatusOK, false},
		{"201 created", http.StatusCreated, false},
		{"301 moved", http.StatusMovedPermanently, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isUpstreamFailureStatus(tc.status)
			if got != tc.want {
				t.Errorf("isUpstreamFailureStatus(%d) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}
