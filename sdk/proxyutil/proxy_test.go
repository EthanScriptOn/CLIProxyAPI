package proxyutil

import (
	"net/http"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Mode
		wantErr bool
	}{
		{name: "inherit", input: "", want: ModeInherit},
		{name: "direct", input: "direct", want: ModeDirect},
		{name: "none", input: "none", want: ModeDirect},
		{name: "http", input: "http://proxy.example.com:8080", want: ModeProxy},
		{name: "https", input: "https://proxy.example.com:8443", want: ModeProxy},
		{name: "socks5", input: "socks5://proxy.example.com:1080", want: ModeProxy},
		{name: "invalid", input: "not-a-proxy", want: ModeInvalid, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.input)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Mode != tt.want {
				t.Fatalf("mode = %v, want %v", got.Mode, tt.want)
			}
		})
	}
}

func TestBuildHTTPTransportDirectBypassesProxy(t *testing.T) {
	transport, mode, errBuild := BuildHTTPTransport("direct")
	if errBuild != nil {
		t.Fatalf("BuildHTTPTransport returned error: %v", errBuild)
	}
	if mode != ModeDirect {
		t.Fatalf("mode = %v, want %v", mode, ModeDirect)
	}
	if transport == nil {
		t.Fatal("expected non-nil transport")
	}
	if transport.Proxy == nil {
		return
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if proxyURL, err := transport.Proxy(req); err != nil {
		t.Fatalf("transport.Proxy returned error: %v", err)
	} else if proxyURL != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestBuildHTTPTransportHTTPProxy(t *testing.T) {
	transport, mode, errBuild := BuildHTTPTransport("http://proxy.example.com:8080")
	if errBuild != nil {
		t.Fatalf("BuildHTTPTransport returned error: %v", errBuild)
	}
	if mode != ModeProxy {
		t.Fatalf("mode = %v, want %v", mode, ModeProxy)
	}
	if transport == nil || transport.Proxy == nil {
		t.Fatal("expected proxy-enabled transport")
	}
}
