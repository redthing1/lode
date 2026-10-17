package kopia_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"math/big"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/contentsession"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	engine "github.com/redthing1/lode/internal/engine/kopia"
)

type remoteFixture struct {
	f         *fixture
	activity  *engine.Activity
	server    *grpc.Server
	info      repo.APIServerInfo
	tls       *tls.Config
	read      atomic.Int64
	written   atomic.Int64
	cutWrite  atomic.Bool
	connected atomic.Int64
	ended     chan struct{}
	mu        sync.Mutex
	conns     []net.Conn
	gets      map[string]bool
}

type measuredListener struct {
	net.Listener
	r *remoteFixture
}

func (l measuredListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.r.mu.Lock()
	l.r.conns = append(l.r.conns, c)
	l.r.mu.Unlock()
	return measuredConn{Conn: c, r: l.r}, nil
}

type measuredConn struct {
	net.Conn
	r *remoteFixture
}

func (c measuredConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.r.read.Add(int64(n))
	return n, err
}

func (c measuredConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.r.written.Add(int64(n))
	return n, err
}

func (r *remoteFixture) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }
func (r *remoteFixture) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (r *remoteFixture) HandleConn(_ context.Context, event stats.ConnStats) {
	switch event.(type) {
	case *stats.ConnBegin:
		r.connected.Add(1)
	case *stats.ConnEnd:
		r.connected.Add(-1)
		select {
		case r.ended <- struct{}{}:
		default:
		}
	}
}

func (r *remoteFixture) HandleRPC(_ context.Context, event stats.RPCStats) {
	in, ok := event.(*stats.InPayload)
	if !ok {
		return
	}
	message, ok := in.Payload.(proto.Message)
	if !ok {
		return
	}
	m := message.ProtoReflect()
	if field := m.Descriptor().Fields().ByName("get_content"); field != nil && m.Has(field) {
		request := m.Get(field).Message()
		id := request.Get(request.Descriptor().Fields().ByName("content_id")).String()
		r.mu.Lock()
		r.gets[id] = true
		r.mu.Unlock()
	}
	if field := m.Descriptor().Fields().ByName("write_content"); field != nil && m.Has(field) && r.cutWrite.CompareAndSwap(true, false) {
		r.mu.Lock()
		for _, c := range r.conns {
			_ = c.Close()
		}
		r.mu.Unlock()
	}
}

func newRemote(t *testing.T, f *fixture) *remoteFixture {
	t.Helper()
	a, err := engine.NewActivity(8)
	if err != nil {
		t.Fatal(err)
	}
	r := &remoteFixture{f: f, activity: a, gets: make(map[string]bool), ended: make(chan struct{}, 32)}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	r.tls = &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.server = grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12})),
		grpc.MaxRecvMsgSize(repo.MaxGRPCMessageSize), grpc.MaxSendMsgSize(repo.MaxGRPCMessageSize), grpc.StatsHandler(r),
	)
	contentsession.Register(r.server, f.rep, func(ctx context.Context) (contentsession.Admission, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		user, token := md.Get("kopia-username"), md.Get("kopia-password")
		if len(user) != 1 || user[0] != "lode" || len(token) != 1 || (token[0] != "proof-append-token" && token[0] != "proof-read-token") {
			return contentsession.Admission{}, status.Error(codes.Unauthenticated, "invalid session token")
		}
		admitted, release, err := a.Enter(ctx)
		if err != nil {
			return contentsession.Admission{}, status.Error(codes.Unavailable, err.Error())
		}
		return contentsession.Admission{Context: admitted, Append: token[0] == "proof-append-token", Release: release}, nil
	})
	r.info = repo.APIServerInfo{BaseURL: "https://" + listener.Addr().String(), TrustedServerCertificateFingerprint: fmtFingerprint(der)}
	done := make(chan error, 1)
	go func() { done <- r.server.Serve(measuredListener{Listener: listener, r: r}) }()
	t.Cleanup(func() {
		r.server.Stop()
		<-done
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.Wait(ctx); err != nil {
			t.Error("engine sessions did not release after shutdown:", err)
		}
	})
	return r
}

func fmtFingerprint(der []byte) string {
	h := sha256.Sum256(der)
	return hex.EncodeToString(h[:])
}

func (r *remoteFixture) open(t *testing.T, name, token string, readOnly bool) repo.Repository {
	t.Helper()
	config := filepath.Join(r.f.dir, name+"-remote.config")
	if err := repo.ConnectAPIServer(r.f.ctx, config, &r.info, token, &repo.ConnectOptions{
		ClientOptions: repo.ClientOptions{Username: "lode", Hostname: name, ReadOnly: readOnly},
		CachingOptions: content.CachingOptions{CacheDirectory: filepath.Join(r.f.dir, name+"-remote-cache"),
			ContentCacheSizeBytes: 1 << 20, MetadataCacheSizeBytes: 1 << 20},
	}); err != nil {
		t.Fatal(err)
	}
	repository, err := repo.Open(r.f.ctx, config, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close(context.Background()) })
	return repository
}
