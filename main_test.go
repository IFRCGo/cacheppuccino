package main

import "testing"

func TestHealthcheckURL(t *testing.T) {
	tests := []struct {
		listenAddr string
		want       string
		wantErr    bool
	}{
		// Wildcard binds are probed via loopback.
		{listenAddr: ":8080", want: "http://127.0.0.1:8080/healthz"},
		{listenAddr: "0.0.0.0:8080", want: "http://127.0.0.1:8080/healthz"},
		{listenAddr: "[::]:8080", want: "http://127.0.0.1:8080/healthz"},
		// Explicit bind hosts are probed directly.
		{listenAddr: "127.0.0.1:9999", want: "http://127.0.0.1:9999/healthz"},
		{listenAddr: "somehost:8081", want: "http://somehost:8081/healthz"},
		{listenAddr: "8080", wantErr: true},
		{listenAddr: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.listenAddr, func(t *testing.T) {
			got, err := healthcheckURL(tt.listenAddr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("healthcheckURL(%q) = %q, want error", tt.listenAddr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("healthcheckURL(%q): %v", tt.listenAddr, err)
			}
			if got != tt.want {
				t.Errorf("healthcheckURL(%q) = %q, want %q", tt.listenAddr, got, tt.want)
			}
		})
	}
}
