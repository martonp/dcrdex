// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package admin provides a password protected https server to send commands to
// a running dex server.
package admin

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/server/account"
	"decred.org/dcrdex/server/asset"
	"decred.org/dcrdex/server/auth"
	"decred.org/dcrdex/server/db"
	dexsrv "decred.org/dcrdex/server/dex"
	"decred.org/dcrdex/server/market"
	"github.com/decred/slog"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

const (
	// rpcTimeoutSeconds is the number of seconds a connection to the
	// server is allowed to stay open without authenticating before it
	// is closed.
	rpcTimeoutSeconds = 10

	marketNameKey      = "market"
	accountIDKey       = "account"
	yesKey             = "yes"
	matchIDKey         = "match"
	assetSymbol        = "asset"
	ruleKey            = "rule"
	scaleKey           = "scale"
	includeInactiveKey = "includeinactive"
	nKey               = "n"
	daysKey            = "days"
	strengthKey        = "strength"
)

var (
	log slog.Logger
)

// SvrCore is satisfied by server/dex.DEX.
type SvrCore interface {
	AccountInfo(acctID account.AccountID) (*db.Account, error)
	UserMatchFails(aid account.AccountID, n int) ([]*auth.MatchFail, error)
	Notify(acctID account.AccountID, msg *msgjson.Message)
	NotifyAll(msg *msgjson.Message)
	ConfigMsg() json.RawMessage
	Asset(id uint32) (*asset.BackedAsset, error)
	SetFeeRateScale(assetID uint32, scale float64)
	ScaleFeeRate(assetID uint32, rate uint64) uint64
	MarketRunning(mktName string) (found, running bool)
	MarketStatus(mktName string) *market.Status
	MarketStatuses() map[string]*market.Status
	SuspendMarket(name string, tSusp time.Time, persistBooks bool) (*market.SuspendEpoch, error)
	ResumeMarket(name string, asSoonAs time.Time) (startEpoch int64, startTime time.Time, err error)
	ForgiveMatchFail(aid account.AccountID, mid order.MatchID) (forgiven, unbanned bool, err error)
	AccountMatchOutcomesN(user account.AccountID, n int) ([]*auth.MatchOutcome, error)
	BookOrders(base, quote uint32) (orders []*order.LimitOrder, err error)
	EpochOrders(base, quote uint32) (orders []order.Order, err error)
	MarketMatchesStreaming(base, quote uint32, includeInactive bool, N int64, f func(*dexsrv.MatchData) error) (int, error)
	EnableDataAPI(yes bool)
	CreatePrepaidBonds(n int, strength uint32, durSecs int64) ([][]byte, error)
	ForgiveUser(user account.AccountID) error
}

// Server is a multi-client https server.
type Server struct {
	core      SvrCore
	addr      string
	tlsConfig *tls.Config
	srv       *http.Server
	authSHA   [32]byte
}

// SrvConfig holds variables needed to create a new Server.
type SrvConfig struct {
	Core            SvrCore
	Addr, Cert, Key string
	AuthSHA         [32]byte
	NoTLS           bool
}

// UseLogger sets the logger for the admin package.
func UseLogger(logger slog.Logger) {
	log = logger
}

// NewServer is the constructor for a new Server.
func NewServer(cfg *SrvConfig) (*Server, error) {
	// Find the key pair.
	if !dex.FileExists(cfg.Key) || !dex.FileExists(cfg.Cert) {
		return nil, fmt.Errorf("missing certificates")
	}

	var tlsConfig *tls.Config
	if !cfg.NoTLS {
		keypair, err := tls.LoadX509KeyPair(cfg.Cert, cfg.Key)
		if err != nil {
			return nil, err
		}

		// Prepare the TLS configuration.
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{keypair},
			MinVersion:   tls.VersionTLS12,
		}
	}

	// Create an HTTP router.
	mux := chi.NewRouter()
	httpServer := &http.Server{
		Handler:      mux,
		ReadTimeout:  rpcTimeoutSeconds * time.Second, // slow requests should not hold connections opened
		WriteTimeout: rpcTimeoutSeconds * time.Second, // hung responses must die
	}

	// Make the server.
	s := &Server{
		core:      cfg.Core,
		srv:       httpServer,
		addr:      cfg.Addr,
		tlsConfig: tlsConfig,
		authSHA:   cfg.AuthSHA,
	}

	// Middleware
	mux.Use(middleware.Recoverer)
	mux.Use(middleware.RealIP)
	mux.Use(oneTimeConnection)
	mux.Use(s.authMiddleware)

	// api endpoints
	mux.Route("/api", func(r chi.Router) {
		r.Use(middleware.AllowContentType("text/plain"))
		r.Get("/ping", apiPing)
		r.Get("/config", s.apiConfig)
		r.Get("/enabledataapi/{"+yesKey+"}", s.apiEnableDataAPI)
		r.Route("/account/{"+accountIDKey+"}", func(rm chi.Router) {
			rm.Get("/", s.apiAccountInfo)
			rm.Get("/outcomes", s.apiMatchOutcomes)
			rm.Get("/fails", s.apiMatchFails)
			rm.Get("/forgive_user", s.forgiveUser)
			rm.Get("/forgive_match/{"+matchIDKey+"}", s.apiForgiveMatchFail)
			rm.Post("/notify", s.apiNotify)
		})
		r.Route("/asset/{"+assetSymbol+"}", func(rm chi.Router) {
			rm.Get("/", s.apiAsset)
			rm.Get("/setfeescale/{"+scaleKey+"}", s.apiSetFeeScale)
		})
		r.Post("/notifyall", s.apiNotifyAll)
		r.Get("/markets", s.apiMarkets)
		r.Route("/market/{"+marketNameKey+"}", func(rm chi.Router) {
			rm.Get("/", s.apiMarketInfo)
			rm.Get("/orderbook", s.apiMarketOrderBook)
			rm.Get("/epochorders", s.apiMarketEpochOrders)
			rm.Get("/matches", s.apiMarketMatches)
			rm.Get("/suspend", s.apiSuspend)
			rm.Get("/resume", s.apiResume)
		})
		r.Get("/prepaybonds", s.prepayBonds)
	})

	return s, nil
}

// Run starts the server.
func (s *Server) Run(ctx context.Context) {
	// Create listener.
	var listener net.Listener
	var err error
	if s.tlsConfig != nil {
		listener, err = tls.Listen("tcp", s.addr, s.tlsConfig)
	} else {
		listener, err = net.Listen("tcp", s.addr)
	}
	if err != nil {
		log.Errorf("can't listen on %s. admin server quitting: %v", s.addr, err)
		return
	}

	// Close the listener on context cancellation.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()

		if err := s.srv.Shutdown(context.Background()); err != nil {
			// Error from closing listeners:
			log.Errorf("HTTP server Shutdown: %v", err)
		}
	}()
	log.Infof("admin server listening on %s", s.addr)
	if err := s.srv.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		log.Warnf("unexpected (http.Server).Serve error: %v", err)
	}

	// Wait for Shutdown.
	wg.Wait()
	log.Infof("admin server off")
}

// oneTimeConnection sets fields in the header and request that indicate this
// connection should not be reused.
func oneTimeConnection(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		r.Close = true
		next.ServeHTTP(w, r)
	})
}

// authMiddleware checks incoming requests for authentication.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// User is ignored.
		_, pass, ok := r.BasicAuth()
		authSHA := sha256.Sum256([]byte(pass))
		if !ok || subtle.ConstantTimeCompare(s.authSHA[:], authSHA[:]) != 1 {
			log.Warnf("server authentication failure from ip: %s", r.RemoteAddr)
			w.Header().Add("WWW-Authenticate", `Basic realm="dex admin"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		log.Infof("server authenticated ip: %s", r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}
