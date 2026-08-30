package comms

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/msgjson"
	"github.com/decred/dcrd/certgen"
	"github.com/gorilla/websocket"
)

var tLogger = dex.StdOutLogger("conn_TEST", dex.LevelTrace)

func makeRequest(id uint64, route string, msg any) *msgjson.Message {
	req, _ := msgjson.NewRequest(id, route, msg)
	return req
}

func testEndpoint(uri string) *WsEndpoint {
	return &WsEndpoint{URL: uri}
}

func newTestWsConn(t *testing.T, cfg *WsCfg) *wsConn {
	t.Helper()
	ws, err := NewWsConn(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ws.(*wsConn)
}

func newTestFailoverWsConn(t *testing.T, cfg *WsCfg, endpoints ...*WsEndpoint) *wsConn {
	t.Helper()
	ws, err := NewFailoverWsConn(cfg, endpoints)
	if err != nil {
		t.Fatal(err)
	}
	return ws.(*wsConn)
}

func TestWsConnInterfaceShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		typ    reflect.Type
		method string
		want   bool
	}{
		{"WsConn has UpdateURL", reflect.TypeOf((*WsConn)(nil)).Elem(), "UpdateURL", true},
		{"WsConn omits SetFailoverEndpoints", reflect.TypeOf((*WsConn)(nil)).Elem(), "SetFailoverEndpoints", false},
		{"FailoverWsConn has SetFailoverEndpoints", reflect.TypeOf((*FailoverWsConn)(nil)).Elem(), "SetFailoverEndpoints", true},
		{"FailoverWsConn omits UpdateURL", reflect.TypeOf((*FailoverWsConn)(nil)).Elem(), "UpdateURL", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, found := tc.typ.MethodByName(tc.method)
			if found != tc.want {
				t.Fatalf("method %q found = %v, want %v", tc.method, found, tc.want)
			}
		})
	}
}

func TestNewWsConnEndpointValidation(t *testing.T) {
	if _, err := NewWsConn(&WsCfg{}); err == nil {
		t.Fatalf("expected empty URL error")
	}
	for _, uri := range []string{
		"http://%zz",
		"https://example.com/ws",
		"wss:///ws",
		"",
	} {
		if _, err := NewWsConn(&WsCfg{URL: uri}); err == nil {
			t.Fatalf("expected invalid endpoint error for %q", uri)
		}
	}

	validEndpoint := []*WsEndpoint{testEndpoint("wss://one.example/ws")}

	if _, err := NewFailoverWsConn(&WsCfg{}, nil); err == nil {
		t.Fatalf("expected empty endpoint list error")
	}
	if _, err := NewFailoverWsConn(&WsCfg{}, []*WsEndpoint{nil}); err == nil {
		t.Fatalf("expected nil endpoint error")
	}
	if _, err := NewFailoverWsConn(&WsCfg{}, []*WsEndpoint{
		testEndpoint("wss://dup.example/ws"),
		testEndpoint("wss://dup.example/ws"),
	}); err == nil {
		t.Fatalf("expected duplicate endpoint URL error")
	}

	for _, tc := range []struct {
		name string
		cfg  *WsCfg
	}{
		{"URL", &WsCfg{URL: "wss://cfg.example/ws"}},
		{"Cert", &WsCfg{Cert: []byte{1}}},
		{"NetDialContext", &WsCfg{NetDialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, nil
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewFailoverWsConn(tc.cfg, validEndpoint); err == nil {
				t.Fatalf("expected %s validation error", tc.name)
			}
		})
	}
}

func TestNewFailoverWsConnCommonConfig(t *testing.T) {
	reconnectSynced := false
	connectEventCalled := false
	rawHandled := false
	headers := http.Header{"X-Test": []string{"one"}}
	cfg := &WsCfg{
		PingWait:         time.Second,
		ReconnectSync:    func() { reconnectSynced = true },
		ConnectEventFunc: func(ConnectionStatus) { connectEventCalled = true },
		Logger:           tLogger,
		RawHandler:       func([]byte) { rawHandled = true },
		ConnectHeaders:   headers,
		EchoPingData:     true,
	}

	conn := newTestFailoverWsConn(t, cfg, testEndpoint("wss://one.example/ws"))
	if conn.cfg == cfg {
		t.Fatalf("failover constructor retained caller cfg")
	}
	if conn.cfg.PingWait != cfg.PingWait ||
		conn.cfg.ConnectHeaders.Get("X-Test") != "one" || !conn.cfg.EchoPingData {
		t.Fatalf("common config fields not copied")
	}
	if conn.cfg.URL != "" || len(conn.cfg.Cert) != 0 || conn.cfg.NetDialContext != nil {
		t.Fatalf("endpoint-specific cfg fields copied into failover config")
	}
	conn.cfg.ReconnectSync()
	conn.cfg.ConnectEventFunc(Connected)
	conn.cfg.RawHandler(nil)
	if !reconnectSynced || !connectEventCalled || !rawHandled {
		t.Fatalf("common callback fields not copied")
	}
}

func TestWsConnSetFailoverEndpoints(t *testing.T) {
	conn := newTestFailoverWsConn(t, &WsCfg{}, testEndpoint("wss://one.example/ws"))

	if err := conn.SetFailoverEndpoints([]*WsEndpoint{
		testEndpoint("wss://one.example/ws"),
		testEndpoint("wss://two.example/ws"),
	}); err != nil {
		t.Fatalf("SetFailoverEndpoints error: %v", err)
	}
	if conn.url() != "wss://one.example/ws" {
		t.Fatalf("active endpoint = %q, want one", conn.url())
	}
	if ep := conn.endpoints.nextEndpoint(); ep.url != "wss://two.example/ws" {
		t.Fatalf("first rotated endpoint = %q, want two", ep.url)
	}
	if ep := conn.endpoints.nextEndpoint(); ep.url != "wss://one.example/ws" {
		t.Fatalf("second rotated endpoint = %q, want one", ep.url)
	}

	for _, endpoints := range [][]*WsEndpoint{
		{{URL: "https://bad.example/ws"}, {URL: "wss:///missing-host"}},
		{testEndpoint("wss://dup.example/ws"), testEndpoint("wss://dup.example/ws")},
		{{URL: "https://bad.example/ws"}, testEndpoint("wss://three.example/ws")},
	} {
		if err := conn.SetFailoverEndpoints(endpoints); err == nil {
			t.Fatalf("expected invalid endpoint replacement error")
		}
		if conn.url() != "wss://one.example/ws" {
			t.Fatalf("invalid replacement changed active endpoint to %q", conn.url())
		}
	}

	if err := conn.SetFailoverEndpoints([]*WsEndpoint{
		testEndpoint("wss://three.example/ws"),
		testEndpoint("wss://one.example/ws"),
		testEndpoint("wss://four.example/ws"),
	}); err != nil {
		t.Fatalf("valid SetFailoverEndpoints error: %v", err)
	}
	if conn.url() != "wss://one.example/ws" {
		t.Fatalf("active endpoint = %q, want one", conn.url())
	}
	if ep := conn.endpoints.nextEndpoint(); ep.url != "wss://four.example/ws" {
		t.Fatalf("first rotated endpoint = %q, want four", ep.url)
	}
	if ep := conn.endpoints.nextEndpoint(); ep.url != "wss://three.example/ws" {
		t.Fatalf("second rotated endpoint = %q, want three", ep.url)
	}
}

func TestWsConnUpdateURL(t *testing.T) {
	conn := newTestWsConn(t, &WsCfg{URL: "wss://one.example/ws"})

	conn.UpdateURL("wss://two.example/ws")
	if conn.url() != "wss://two.example/ws" {
		t.Fatalf("updated endpoint URL = %q, want two", conn.url())
	}

	conn.UpdateURL("http://%zz")
	if conn.url() != "wss://two.example/ws" {
		t.Fatalf("invalid UpdateURL changed active endpoint to %q", conn.url())
	}
	conn.UpdateURL("https://bad.example/ws")
	if conn.url() != "wss://two.example/ws" {
		t.Fatalf("invalid scheme UpdateURL changed active endpoint to %q", conn.url())
	}
	conn.UpdateURL("wss:///missing-host")
	if conn.url() != "wss://two.example/ws" {
		t.Fatalf("empty host UpdateURL changed active endpoint to %q", conn.url())
	}
}

func TestWsConnIgnoresStaleGeneration(t *testing.T) {
	conn := newTestWsConn(t, &WsCfg{URL: "wss://one.example/ws"})
	atomic.StoreUint64(&conn.connID, 2)
	conn.setConnectionStatus(Connected)

	conn.handleReadError(1, io.EOF)
	if conn.IsDown() {
		t.Fatalf("stale read failure marked current connection down")
	}
	select {
	case <-conn.reconnectCh:
		t.Fatalf("stale read failure scheduled reconnect")
	default:
	}
}

func TestWsConnReadErrorAbortsPendingRequests(t *testing.T) {
	conn := newTestWsConn(t, &WsCfg{URL: "wss://one.example/ws"})
	atomic.StoreUint64(&conn.connID, 1)
	conn.setConnectionStatus(Connected)

	expired := make(chan struct{}, 1)
	conn.logReq(1, func(*msgjson.Message) {}, time.Hour, func() {
		expired <- struct{}{}
	})

	conn.handleReadError(1, io.EOF)
	if !conn.IsDown() {
		t.Fatalf("read error did not mark connection down")
	}
	select {
	case <-expired:
	default:
		t.Fatalf("read error did not abort the pending request")
	}
	if conn.respHandler(1) != nil {
		t.Fatalf("aborted request still registered")
	}
	select {
	case <-conn.reconnectCh:
	default:
		t.Fatalf("read error did not schedule reconnect")
	}

	// A stale generation's read error must NOT abort requests logged by a
	// newer generation.
	atomic.StoreUint64(&conn.connID, 5)
	conn.setConnectionStatus(Connected)
	conn.logReq(2, func(*msgjson.Message) {}, time.Hour, func() {
		expired <- struct{}{}
	})
	conn.handleReadError(3, io.EOF) // stale connID
	select {
	case <-expired:
		t.Fatalf("stale read error aborted a newer generation's request")
	default:
	}
	if conn.respHandler(2) == nil {
		t.Fatalf("stale read error removed a newer generation's request")
	}
}

func TestWsConnRequestSendFailureOnlyUnregistersFailedRequest(t *testing.T) {
	conn := newTestWsConn(t, &WsCfg{URL: "wss://one.example/ws"})
	conn.setConnectionStatus(Connected)

	conn.logReq(1, func(*msgjson.Message) {}, time.Hour, func() {})

	err := conn.RequestRawWithTimeout(2, []byte(`{"type":1}`), func(*msgjson.Message) {
		t.Fatalf("failed request response handler should not run")
	}, time.Hour, func() {
		t.Fatalf("failed request expire should not run")
	})
	if err == nil {
		t.Fatalf("expected write error with no websocket")
	}
	if conn.respHandler(1) == nil {
		t.Fatalf("send failure removed existing response handler")
	}
	if conn.respHandler(2) != nil {
		t.Fatalf("failed request response handler still registered")
	}
	if conn.IsDown() {
		t.Fatalf("send failure marked connection down")
	}
}

func TestWsConnFailover(t *testing.T) {
	var wg sync.WaitGroup
	defer wg.Wait()

	upgrader := websocket.Upgrader{}

	type wsServer struct {
		*httptest.Server
		mtx      sync.Mutex
		conns    []*websocket.Conn
		accepted chan struct{}
	}
	// killConns force-closes the server's live websocket connections without
	// stopping the server, simulating a node failure with fast recovery.
	killConns := func(s *wsServer) {
		s.mtx.Lock()
		defer s.mtx.Unlock()
		for _, c := range s.conns {
			c.Close()
		}
		s.conns = nil
	}
	newServer := func() *wsServer {
		s := &wsServer{accepted: make(chan struct{}, 1)}
		s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade error: %v", err)
				return
			}
			s.mtx.Lock()
			s.conns = append(s.conns, c)
			s.mtx.Unlock()
			s.accepted <- struct{}{}
			// Ping periodically so the client's read deadline stays fresh, and
			// consume inbound messages until the connection dies.
			wg.Add(2)
			go func() {
				defer wg.Done()
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()
				for range ticker.C {
					if c.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)) != nil {
						return
					}
				}
			}()
			go func() {
				defer wg.Done()
				for {
					if _, _, err := c.ReadMessage(); err != nil {
						return
					}
				}
			}()
		}))
		return s
	}
	wsURL := func(s *wsServer) string { return "ws" + strings.TrimPrefix(s.URL, "http") }

	serverA, serverB := newServer(), newServer()
	defer serverA.Close()
	defer serverB.Close()

	statusCh := make(chan ConnectionStatus, 16)
	conn := newTestFailoverWsConn(t, &WsCfg{
		PingWait:         time.Second,
		ConnectEventFunc: func(status ConnectionStatus) { statusCh <- status },
		Logger:           tLogger,
	}, testEndpoint(wsURL(serverA)), testEndpoint(wsURL(serverB)))

	ctx, cancel := context.WithCancel(t.Context())
	connWG, err := conn.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer func() {
		cancel()
		connWG.Wait()
	}()

	waitStatus := func(tag string, want ConnectionStatus) {
		t.Helper()
		timeout := time.After(10 * time.Second)
		for {
			select {
			case status := <-statusCh:
				if status == want {
					return
				}
			case <-timeout:
				t.Fatalf("%s: no %v connection event", tag, want)
			}
		}
	}
	waitAccepted := func(tag string, s *wsServer) {
		t.Helper()
		select {
		case <-s.accepted:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: server did not record connection", tag)
		}
	}

	waitStatus("initial connect", Connected)
	waitAccepted("initial connect", serverA)
	if active := conn.ActiveEndpoint(); active != wsURL(serverA) {
		t.Fatalf("initial active endpoint = %q, want server A %q", active, wsURL(serverA))
	}

	// An in-flight request should be aborted promptly when the connection
	// fails, not left to wait out its expiry timer.
	expired := make(chan struct{}, 1)
	req := makeRequest(conn.NextID(), "test", nil)
	if err := conn.RequestWithTimeout(req, func(*msgjson.Message) {
		t.Errorf("no response expected for the abandoned request")
	}, time.Hour, func() { expired <- struct{}{} }); err != nil {
		t.Fatalf("RequestWithTimeout error: %v", err)
	}

	// Kill server A's connections. The client should fail over to server B.
	killConns(serverA)
	waitStatus("failover", Connected)
	waitAccepted("failover", serverB)
	if active := conn.ActiveEndpoint(); active != wsURL(serverB) {
		t.Fatalf("post-failover active endpoint = %q, want server B %q", active, wsURL(serverB))
	}
	select {
	case <-expired:
	case <-time.After(10 * time.Second):
		t.Fatalf("in-flight request not aborted on failover")
	}

	// Kill server B's connections. The client should rotate back to server A.
	killConns(serverB)
	waitStatus("failback", Connected)
	waitAccepted("failback", serverA)
	if active := conn.ActiveEndpoint(); active != wsURL(serverA) {
		t.Fatalf("post-failback active endpoint = %q, want server A %q", active, wsURL(serverA))
	}
}

// genCertPair generates a key/cert pair to the paths provided.
func genCertPair(certFile, keyFile string, altDNSNames []string) error {
	tLogger.Infof("Generating TLS certificates...")

	org := "dcrdex autogenerated cert"
	validUntil := time.Now().Add(10 * 365 * 24 * time.Hour)
	cert, key, err := certgen.NewTLSCertPair(elliptic.P521(), org,
		validUntil, altDNSNames)
	if err != nil {
		return err
	}

	// Write cert and key files.
	if err = os.WriteFile(certFile, cert, 0644); err != nil {
		return err
	}
	if err = os.WriteFile(keyFile, key, 0600); err != nil {
		os.Remove(certFile)
		return err
	}

	tLogger.Infof("Done generating TLS certificates")
	return nil
}

func TestWsConn(t *testing.T) {
	// Must wait for goroutines, especially the ones that capture t.
	var wg sync.WaitGroup
	defer wg.Wait()

	upgrader := websocket.Upgrader{}

	pingCh := make(chan struct{})
	readPumpCh := make(chan any)
	writePumpCh := make(chan *msgjson.Message)
	ctx := t.Context()

	type conn struct {
		sync.WaitGroup
		*websocket.Conn
	}
	var clientMtx sync.Mutex
	clients := make(map[uint64]*conn)

	// server.Shutdown does not wait for hijacked connections, and pong handler
	// uses t.Logf.
	defer func() {
		clientMtx.Lock()
		for id, h := range clients {
			h.Close()
			h.Wait()
			delete(clients, id)
		}
		clientMtx.Unlock()
	}()

	var id uint64
	// server's "/ws" handler
	handler := func(w http.ResponseWriter, r *http.Request) {
		t.Helper()
		id := atomic.AddUint64(&id, 1) // shadow id
		hCtx, hCancel := context.WithCancel(ctx)

		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("unable to upgrade http connection: %s", err)
		}

		ch := &conn{Conn: c}
		clientMtx.Lock()
		clients[id] = ch
		clientMtx.Unlock()

		c.SetPongHandler(func(string) error {
			t.Logf("handler #%d: pong received", id)
			return nil
		})

		ch.Add(1)
		go func() {
			defer ch.Done()
			for {
				select {
				case <-pingCh:
					err := c.WriteControl(websocket.PingMessage, []byte{},
						time.Now().Add(writeWait))
					if err != nil {
						if hCtx.Err() == nil {
							// Only a failure if the server isn't shutting down.
							t.Errorf("handler #%d: ping error: %v", id, err)
						}
						return
					}

					t.Logf("handler #%d: ping sent", id)

				case msg := <-readPumpCh:
					err := c.WriteJSON(msg)
					if err != nil {
						t.Errorf("handler #%d: write error: %v", id, err)
						return
					}

				case <-hCtx.Done():
					return
				}
			}
		}()

		ch.Add(1)
		go func() {
			defer ch.Done()
			for {
				mType, message, err := c.ReadMessage()
				if err != nil {
					hCancel()
					c.Close()

					// If the context has been canceled, don't do anything.
					if hCtx.Err() != nil {
						return
					}

					if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
						// Terminate on a normal close message.
						return
					}

					t.Errorf("handler #%d: read error: %v\n", id, err)
					return
				}

				if mType == websocket.TextMessage {
					msg, err := msgjson.DecodeMessage(message)
					if err != nil {
						t.Errorf("handler #%d: decode error: %v", id, err)
						continue // Don't hang up.
					}

					writePumpCh <- msg
				}
			}
		}()
	}

	certFile, err := os.CreateTemp("", "certfile")
	if err != nil {
		t.Fatalf("unable to create temp certfile: %s", err)
	}
	certFile.Close()
	defer os.Remove(certFile.Name())

	keyFile, err := os.CreateTemp("", "keyfile")
	if err != nil {
		t.Fatalf("unable to create temp keyfile: %s", err)
	}
	keyFile.Close()
	defer os.Remove(keyFile.Name())

	err = genCertPair(certFile.Name(), keyFile.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}

	certB, err := os.ReadFile(certFile.Name())
	if err != nil {
		t.Fatalf("file reading error: %v", err)
	}

	host := "127.0.0.1:0"
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", handler)

	// http server for the connect and upgrade
	server := &http.Server{
		WriteTimeout: time.Second * 10,
		ReadTimeout:  time.Second * 10,
		Addr:         host,
		Handler:      mux,
	}
	defer server.Shutdown(context.Background())

	wg.Add(1)
	serverReady := make(chan error, 1)
	go func() {
		defer wg.Done()

		ln, err := net.Listen("tcp", server.Addr)
		if err != nil {
			serverReady <- err
			return
		}
		defer ln.Close()
		//log.Info(ln.Addr().(*net.TCPAddr).Port)
		host = ln.Addr().String()
		serverReady <- nil // after setting host

		err = server.ServeTLS(ln, certFile.Name(), keyFile.Name())
		if err != nil {
			fmt.Println(err)
		}
	}()

	// wait for server to start listening before connecting
	err = <-serverReady
	if err != nil {
		t.Fatal(err)
	}

	const pingWait = 500 * time.Millisecond
	setupWsConn := func(cert []byte) (*wsConn, error) {
		cfg := &WsCfg{
			URL:      "wss://" + host + "/ws",
			Cert:     cert,
			Logger:   tLogger,
			PingWait: pingWait,
		}
		conn, err := NewWsConn(cfg)
		if err != nil {
			return nil, err
		}
		return conn.(*wsConn), nil
	}

	// test no cert error
	noCertConn, err := setupWsConn(nil)
	if err != nil {
		t.Fatal(err)
	}
	noCertConnMaster := dex.NewConnectionMaster(noCertConn)
	err = noCertConnMaster.Connect(ctx)
	noCertConnMaster.Disconnect()
	if err == nil || !errors.Is(err, ErrCertRequired) {
		t.Fatalf("failed to get ErrCertRequired for no cert connection, got %v", err)
	}

	// test invalid cert error
	_, err = setupWsConn([]byte("invalid cert"))
	if err == nil || !errors.Is(err, ErrInvalidCert) {
		t.Fatalf("failed to get ErrInvalidCert for invalid cert connection, got %v", err)
	}

	// connect with cert
	wsc, err := setupWsConn(certB)
	if err != nil {
		t.Fatal(err)
	}
	waiter := dex.NewConnectionMaster(wsc)
	err = waiter.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	reconnectAndPing := func() {
		// Drop the connection and force a reconnect by waiting longer than the
		// read deadline (the ping wait), plus a bit extra to allow the timeout
		// to flip off the connection and queue a reconnect.
		time.Sleep(pingWait * 3 / 2)
		runtime.Gosched()

		// Wait for a reconnection.
		for wsc.IsDown() {
			time.Sleep(time.Millisecond * 10)
			continue
		}

		// Send a ping.
		pingCh <- struct{}{}
	}

	orderid, _ := hex.DecodeString("ceb09afa675cee31c0f858b94c81bd1a4c2af8c5947d13e544eef772381f2c8d")
	matchid, _ := hex.DecodeString("7c6b44735e303585d644c713fe0e95897e7e8ba2b9bba98d6d61b70006d3d58c")
	match := &msgjson.Match{
		OrderID:  orderid,
		MatchID:  matchid,
		Quantity: 20,
		Rate:     2,
		Address:  "DsiNAJCd2sSazZRU9ViDD334DaLgU1Kse3P",
	}

	// Ensure a malformed message to the client does not terminate
	// the connection.
	readPumpCh <- []byte("{notjson")

	// Send a message to the client.
	sent := makeRequest(1, msgjson.MatchRoute, match)
	readPumpCh <- sent

	// Fetch the read source.
	readSource := wsc.MessageSource()
	if readSource == nil {
		t.Fatal("expected a non-nil read source")
	}

	// Read the message received by the client.
	received := <-readSource

	// Ensure the received message equal to the sent message.
	if received.Type != sent.Type {
		t.Fatalf("expected %v type, got %v", sent.Type, received.Type)
	}

	if received.Route != sent.Route {
		t.Fatalf("expected %v route, got %v", sent.Route, received.Route)
	}

	if received.ID != sent.ID {
		t.Fatalf("expected %v id, got %v", sent.ID, received.ID)
	}

	if !bytes.Equal(received.Payload, sent.Payload) {
		t.Fatal("sent and received payload mismatch")
	}

	reconnectAndPing()

	coinID := []byte{
		0xc3, 0x16, 0x10, 0x33, 0xde, 0x09, 0x6f, 0xd7, 0x4d, 0x90, 0x51, 0xff,
		0x0b, 0xd9, 0x9e, 0x35, 0x9d, 0xe3, 0x50, 0x80, 0xa3, 0x51, 0x10, 0x81,
		0xed, 0x03, 0x5f, 0x54, 0x1b, 0x85, 0x0d, 0x43, 0x00, 0x00, 0x00, 0x0a,
	}

	contract, _ := hex.DecodeString("caf8d277f80f71e4")
	init := &msgjson.Init{
		OrderID:  orderid,
		MatchID:  matchid,
		CoinID:   coinID,
		Contract: contract,
	}

	// Send a message from the client.
	mId := wsc.NextID()
	sent = makeRequest(mId, msgjson.InitRoute, init)
	handlerRun := false
	err = wsc.Request(sent, func(*msgjson.Message) {
		handlerRun = true
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Read the message received by the server.
	received = <-writePumpCh

	// Ensure the received message equal to the sent message.
	if received.Type != sent.Type {
		t.Fatalf("expected %v type, got %v", sent.Type, received.Type)
	}

	if received.Route != sent.Route {
		t.Fatalf("expected %v route, got %v", sent.Route, received.Route)
	}

	if received.ID != sent.ID {
		t.Fatalf("expected %v id, got %v", sent.ID, received.ID)
	}

	if !bytes.Equal(received.Payload, sent.Payload) {
		t.Fatal("sent and received payload mismatch")
	}

	// Ensure the next id is as expected.
	next := wsc.NextID()
	if next != 2 {
		t.Fatalf("expected next id to be %d, got %d", 2, next)
	}

	// Ensure the request got logged, also unregister the response handler.
	hndlr := wsc.respHandler(mId)
	if hndlr == nil {
		t.Fatalf("no handler found")
	}
	hndlr.f(nil)
	if !handlerRun {
		t.Fatalf("wrong handler retrieved")
	}

	// Ensure the response handler is unlogged.
	hndlr = wsc.respHandler(mId)
	if hndlr != nil {
		t.Fatal("found a response handler for an unlogged request id")
	}

	pingCh <- struct{}{}

	// Ensure malformed request data (a send failure) does not leave a
	// registered response handler or kill the connection.
	sent.ID = wsc.NextID()
	sent.Payload = []byte("{notjson")
	err = wsc.Request(sent, func(*msgjson.Message) {})
	if err == nil {
		t.Fatalf("expected error with malformed request payload")
	}

	// Ensure the response handler is unregistered.
	if wsc.respHandler(mId) != nil {
		t.Fatal("response handler was still registered")
	}

	// New request to test expiration.
	mId = next
	sent = makeRequest(mId, msgjson.InitRoute, init)
	expiring := make(chan struct{}, 1)
	expTime := 50 * time.Millisecond // way shorter than pingWait
	err = wsc.RequestWithTimeout(sent, func(*msgjson.Message) {}, expTime, func() {
		expiring <- struct{}{}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	<-writePumpCh

	pingCh <- struct{}{}

	// Yield to the comms goroutine in case this machine is poor.
	runtime.Gosched()
	select {
	case <-expiring:
	case <-time.NewTimer(time.Second).C: // >> expTime
		t.Fatalf("didn't expire") // conn will be dead by this time without pings
	}

	// New request to abort on conn shutdown.
	sent = makeRequest(wsc.NextID(), msgjson.InitRoute, init)
	expiring = make(chan struct{}, 1)
	expTime = 20 * time.Second                  // we're going to cancel first
	beforeExpire := time.After(2 * time.Second) // enough time for shutdown to call expire func
	err = wsc.RequestWithTimeout(sent, func(*msgjson.Message) {}, expTime, func() {
		expiring <- struct{}{}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	<-writePumpCh

	pingCh <- struct{}{}

	// Shutdown/Disconnect before expire.
	time.Sleep(50 * time.Millisecond) // let pings and pongs flush, but it's not a problem if they bomb
	waiter.Disconnect()

	select {
	case <-beforeExpire: // much shorter than req timeout
		t.Error("expire func not called on conn shutdown")
	case <-expiring: // means aborted if triggered before timeout
	}

	select {
	case _, ok := <-readSource:
		if ok {
			t.Error("read source should have been closed")
		}
	default:
		t.Error("read source should have been closed")
	}
}
