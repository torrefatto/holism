// Command mind is the Holism central coordinator (single instance). It serves
// MindService over mTLS — the swarm's only network endpoint.
//
// Configuration is 100% environment variables (see docs/design §6):
//
//	MIND_ADDR      listen address (default ":8443")
//	MIND_TLS_CERT  path to the mind's server certificate
//	MIND_TLS_KEY   path to the mind's server key
//	MIND_TLS_CA    path to the root CA that verifies client certificates
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
	eventbusmemory "github.com/torrefatto/holism/mind/internal/eventbus/memory"
	"github.com/torrefatto/holism/mind/internal/planner/rules"
	registrymemory "github.com/torrefatto/holism/mind/internal/registry/memory"
	"github.com/torrefatto/holism/mind/internal/server"
	taskstorememory "github.com/torrefatto/holism/mind/internal/taskstore/memory"
)

func main() {
	addr := os.Getenv("MIND_ADDR")
	if addr == "" {
		addr = ":8443"
	}
	tlsCfg, err := server.TLSConfigFromEnv()
	if err != nil {
		log.Fatalf("mind: %v", err)
	}

	srv := server.New(server.Deps{
		Store:    taskstorememory.New(nil, nil),
		Bus:      eventbusmemory.New(nil, nil),
		Planner:  rules.New(),
		Registry: registrymemory.New(nil, 0),
	})
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	holismv1.RegisterMindServiceServer(grpcServer, srv)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("mind: listen %s: %v", addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	log.Printf("mind: serving MindService on %s (mTLS)", lis.Addr())
	if err := grpcServer.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		log.Fatalf("mind: serve: %v", err)
	}
}
