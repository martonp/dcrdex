// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dex

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestMeshClientEndpoint(t *testing.T) {
	certPath := filepath.Join(t.TempDir(), "rpc.cert")
	cert := []byte("cert")
	if err := os.WriteFile(certPath, cert, 0600); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	missingCert := filepath.Join(t.TempDir(), "missing.cert")
	tests := []struct {
		name     string
		address  string
		noTLS    bool
		certPath string
		wantHost string
		wantCert []byte
		wantErr  bool
	}{
		{
			name: "TLS hostname", address: "DEX.EXAMPLE.COM:7232",
			certPath: certPath, wantHost: "DEX.EXAMPLE.COM:7232", wantCert: cert,
		},
		{
			name: "onion URL", address: "wss://ABC.ONION/ws",
			certPath: missingCert, wantHost: "wss://ABC.ONION/ws",
		},
		{
			name: "plaintext", address: "dex.example.com:7232", noTLS: true,
			certPath: missingCert, wantHost: "dex.example.com:7232",
		},
		{
			name: "URL with whitespace", address: " wss://dex.example.com/ws ",
			certPath: certPath, wantHost: "wss://dex.example.com/ws", wantCert: cert,
		},
		{
			name: "onion substring in ordinary hostname", address: "dex.onion.example.com:7232",
			certPath: certPath, wantHost: "dex.onion.example.com:7232", wantCert: cert,
		},
		{
			name: "missing certificate", address: "dex.example.com:7232",
			certPath: missingCert, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, gotCert, err := meshClientEndpoint(tt.address, tt.noTLS, tt.certPath)
			if (err != nil) != tt.wantErr {
				t.Fatalf("meshClientEndpoint error = %v, want error %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if host != tt.wantHost || !bytes.Equal(gotCert, tt.wantCert) {
				t.Fatalf("endpoint = %q/%x, want %q/%x", host, gotCert, tt.wantHost, tt.wantCert)
			}
		})
	}
}
