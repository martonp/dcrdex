// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dex

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"decred.org/dcrdex/dex/msgjson"
)

func TestMeshClientEndpoint(t *testing.T) {
	certPath := filepath.Join(t.TempDir(), "rpc.cert")
	cert := []byte("cert")
	if err := os.WriteFile(certPath, cert, 0600); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	host, gotCert, err := meshClientEndpoint("DEX.EXAMPLE.COM:7232", false, certPath)
	if err != nil {
		t.Fatalf("meshClientEndpoint error: %v", err)
	}
	if host != "DEX.EXAMPLE.COM:7232" || !bytes.Equal(gotCert, cert) {
		t.Fatalf("endpoint = %q/%x", host, gotCert)
	}

	host, gotCert, err = meshClientEndpoint("wss://ABC.ONION/ws", false, "missing.cert")
	if err != nil {
		t.Fatalf("onion endpoint error: %v", err)
	}
	if host != "wss://ABC.ONION/ws" || len(gotCert) != 0 {
		t.Fatalf("onion endpoint = %q/%x", host, gotCert)
	}

	host, gotCert, err = meshClientEndpoint("dex.example.com:7232", true, "missing.cert")
	if err != nil {
		t.Fatalf("noTLS endpoint error: %v", err)
	}
	if host != "dex.example.com:7232" || len(gotCert) != 0 {
		t.Fatalf("noTLS endpoint = %q/%x", host, gotCert)
	}

	host, gotCert, err = meshClientEndpoint(" wss://dex.example.com/ws ", false, certPath)
	if err != nil {
		t.Fatalf("url endpoint error = %v", err)
	}
	if host != "wss://dex.example.com/ws" || !bytes.Equal(gotCert, cert) {
		t.Fatalf("url endpoint = %q/%x", host, gotCert)
	}
}

func TestPublishMeshClientEndpoints(t *testing.T) {
	var broadcasts []*msgjson.Message
	dm := &DEX{
		configResp: &configResponse{},
		broadcast: func(msg *msgjson.Message) {
			broadcasts = append(broadcasts, msg)
		},
	}
	dm.configResp.configMsg = new(msgjson.ConfigResult)

	lastNote := func() *msgjson.MeshEndpointsNotification {
		t.Helper()
		msg := broadcasts[len(broadcasts)-1]
		if msg.Route != msgjson.MeshEndpointsRoute {
			t.Fatalf("broadcast route = %q, expected %q", msg.Route, msgjson.MeshEndpointsRoute)
		}
		note := new(msgjson.MeshEndpointsNotification)
		if err := msg.Unmarshal(note); err != nil {
			t.Fatalf("notification unmarshal error: %v", err)
		}
		return note
	}

	ownCert := []byte{4, 5, 6}
	peerCert := []byte{1, 2, 3}
	dm.publishMeshClientEndpoints("self.example:7232", ownCert, "peer.example:7232", peerCert)
	peerCert[0] = 9

	// The advertisement must carry the node's own endpoint first, then the peer's.
	endpoints := dm.configResp.configMsg.MeshEndpoints
	if len(endpoints) != 2 || endpoints[0].Host != "self.example:7232" || !bytes.Equal(endpoints[0].Cert, []byte{4, 5, 6}) ||
		endpoints[1].Host != "peer.example:7232" || !bytes.Equal(endpoints[1].Cert, []byte{1, 2, 3}) {
		t.Fatalf("mesh endpoints = %+v", endpoints)
	}
	if len(dm.configResp.configEnc) == 0 {
		t.Fatal("config response was not remarshaled")
	}

	// Setting the endpoints must broadcast exactly one notification.
	if len(broadcasts) != 1 {
		t.Fatalf("%d broadcasts after set, expected 1", len(broadcasts))
	}
	note := lastNote()
	if len(note.MeshEndpoints) != 2 || note.MeshEndpoints[0].Host != "self.example:7232" ||
		note.MeshEndpoints[1].Host != "peer.example:7232" ||
		!bytes.Equal(note.MeshEndpoints[1].Cert, []byte{1, 2, 3}) {
		t.Fatalf("notification endpoints = %+v", note.MeshEndpoints)
	}

	// Re-setting the same endpoints must not remarshal or broadcast.
	enc := string(dm.configResp.configEnc)
	dm.publishMeshClientEndpoints("self.example:7232", []byte{4, 5, 6}, "peer.example:7232", []byte{1, 2, 3})
	if len(broadcasts) != 1 {
		t.Fatalf("%d broadcasts after unchanged set, expected 1", len(broadcasts))
	}
	if string(dm.configResp.configEnc) != enc {
		t.Fatal("config response remarshaled for an unchanged endpoint set")
	}

	// A peer whose host duplicates the node's own endpoint is advertised once.
	dm.publishMeshClientEndpoints("self.example:7232", []byte{4, 5, 6}, "self.example:7232", []byte{4, 5, 6})
	if endpoints = dm.configResp.configMsg.MeshEndpoints; len(endpoints) != 1 || endpoints[0].Host != "self.example:7232" {
		t.Fatalf("mesh endpoints with duplicate peer = %+v", endpoints)
	}
	if len(broadcasts) != 2 {
		t.Fatalf("%d broadcasts after duplicate-peer set, expected 2", len(broadcasts))
	}

	// Restore own+peer, then drop the peer: still advertise the node's own.
	dm.publishMeshClientEndpoints("self.example:7232", []byte{4, 5, 6}, "peer.example:7232", []byte{1, 2, 3})
	dm.publishMeshClientEndpoints("self.example:7232", []byte{4, 5, 6}, "", nil)
	if endpoints = dm.configResp.configMsg.MeshEndpoints; len(endpoints) != 1 || endpoints[0].Host != "self.example:7232" {
		t.Fatalf("mesh endpoints without peer = %+v", endpoints)
	}
	if len(broadcasts) != 4 {
		t.Fatalf("%d broadcasts after peer drop, expected 4", len(broadcasts))
	}

	// An empty own host is rejected: no config change and no broadcast.
	enc = string(dm.configResp.configEnc)
	broadcastCount := len(broadcasts)
	dm.publishMeshClientEndpoints("", nil, "peer.example:7232", []byte{1, 2, 3})
	if len(broadcasts) != broadcastCount {
		t.Fatalf("%d broadcasts after empty own host, expected %d", len(broadcasts), broadcastCount)
	}
	if string(dm.configResp.configEnc) != enc {
		t.Fatal("config response remarshaled for empty own host")
	}
	if endpoints = dm.configResp.configMsg.MeshEndpoints; len(endpoints) != 1 || endpoints[0].Host != "self.example:7232" {
		t.Fatalf("mesh endpoints after empty own host = %+v", endpoints)
	}
}
