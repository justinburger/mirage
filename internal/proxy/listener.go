package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/netutil"
)

// Listener settings.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 1 << 20
	// drainTimeout bounds how long a replaced or removed listener lets its
	// in-flight requests finish.
	drainTimeout = 5 * time.Second
	// selfSignedLifetime is the validity of generated certificates. They are
	// regenerated at every start; a long life avoids a surprise expiry in a
	// service that runs for years.
	selfSignedLifetime = 10 * 365 * 24 * time.Hour
)

// listener is one bound socket with its HTTP server.
type listener struct {
	srv      *http.Server
	newConns *netutil.NewConns // connections that sent no request yet
	ln       net.Listener
	tls      bool
	addr     string // resolved address
	done     chan struct{}
	stopping atomic.Bool
}

// bind opens svc.ListenAddr and prepares the HTTP server, without serving
// yet: the caller publishes the service state first and then calls serve, so
// no request can arrive before the service can answer it. Bind and certificate
// errors are returned before anything is started.
func (s *service) bind(svc model.Service) (*listener, error) {
	m := s.m
	var tlsCfg *tls.Config
	if svc.TLS != nil {
		var err error
		if tlsCfg, err = tlsConfig(svc.TLS, svc.ListenAddr); err != nil {
			return nil, fmt.Errorf("tls: %w", err)
		}
	}
	ln, err := net.Listen("tcp", svc.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", svc.ListenAddr, err)
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	if tlsCfg != nil {
		protocols.SetHTTP2(true)
	} else {
		protocols.SetUnencryptedHTTP2(true)
	}
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		TLSConfig:         tlsCfg,
		Protocols:         protocols,
		BaseContext:       func(net.Listener) context.Context { return m.root },
		ErrorLog:          m.httpLog,
	}
	return &listener{srv: srv, newConns: netutil.Track(srv), ln: ln, tls: tlsCfg != nil, addr: ln.Addr().String(), done: make(chan struct{})}, nil
}

// serve starts accepting connections on l.
func (s *service) serve(l *listener) {
	m := s.m
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(l.done)
		var err error
		if l.tls {
			err = l.srv.ServeTLS(l.ln, "", "")
		} else {
			err = l.srv.Serve(l.ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !l.stopping.Load() {
			m.log.Error("listener failed", "service", s.id, "addr", l.addr, "error", err)
			s.listenerFailed(l, err)
		}
	}()
}

// beginStop closes the socket so no new connection is accepted; requests in
// flight are not affected.
func (l *listener) beginStop() {
	l.stopping.Store(true)
	_ = l.ln.Close()
}

// shutdown gracefully stops the server and waits for its requests, bounded by
// ctx. beginStop already closed the socket, so net.ErrClosed from
// http.Server.Shutdown (it returns the close error of a listener it still
// tracked, after the connections drained) only says the socket was released
// first; it is not a failure.
func (l *listener) shutdown(ctx context.Context) error {
	// A connection that never sent a request (a client's speculative dial)
	// would otherwise hold Shutdown for five seconds.
	l.newConns.CloseAll()
	if err := l.srv.Shutdown(ctx); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// drain shuts the server down in the background, giving requests in flight
// drainTimeout to finish, and returns at once.
func (m *Manager) drain(l *listener) {
	l.beginStop()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		if err := l.shutdown(ctx); err != nil {
			_ = l.srv.Close()
		}
		<-l.done
	}()
}

// sameSocket reports whether two listen addresses denote the same socket, so
// a replacement cannot be bound before the old listener is closed. Port 0
// never collides.
func sameSocket(a, b string) bool {
	ha, pa, erra := net.SplitHostPort(a)
	hb, pb, errb := net.SplitHostPort(b)
	if erra != nil || errb != nil || pa != pb || pa == "0" {
		return false
	}
	wild := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" }
	if wild(ha) && wild(hb) {
		return true
	}
	return ha == hb
}

// tlsConfig builds the server TLS configuration: the certificate and key
// files when given, otherwise a generated self-signed certificate.
func tlsConfig(t *model.TLSConfig, listenAddr string) (*tls.Config, error) {
	var cert tls.Certificate
	var err error
	switch {
	case t.CertFile != "" && t.KeyFile != "":
		cert, err = tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
	case t.SelfSigned:
		host, _, _ := net.SplitHostPort(listenAddr)
		cert, err = selfSignedCert(host)
	default:
		err = errors.New("needs self_signed or both cert_file and key_file")
	}
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}, nil
}

// selfSignedCert generates an ECDSA P-256 certificate valid for localhost,
// 127.0.0.1, ::1 and host (a DNS name or an IP address; empty and wildcard
// addresses add nothing).
func selfSignedCert(host string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"Mirage"}, CommonName: "mirage self-signed"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	if host != "" {
		if ip := net.ParseIP(host); ip != nil {
			if !ip.IsUnspecified() && !ip.Equal(net.IPv4(127, 0, 0, 1)) && !ip.Equal(net.IPv6loopback) {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			}
		} else if host != "localhost" {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}
