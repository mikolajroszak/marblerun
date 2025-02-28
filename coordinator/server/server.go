// Copyright (c) Edgeless Systems GmbH.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package server contains the ClientAPI HTTP-REST and MarbleAPI gRPC server.
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"

	"github.com/edgelesssys/marblerun/coordinator/core"
	"github.com/edgelesssys/marblerun/coordinator/events"
	"github.com/edgelesssys/marblerun/coordinator/manifest"
	"github.com/edgelesssys/marblerun/coordinator/rpc"
	"github.com/edgelesssys/marblerun/coordinator/state"
	"github.com/edgelesssys/marblerun/coordinator/user"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_zap "github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	grpc_ctxtags "github.com/grpc-ecosystem/go-grpc-middleware/tags"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type clientAPI interface {
	SetManifest(ctx context.Context, rawManifest []byte) (recoverySecretMap map[string][]byte, err error)
	GetCertQuote(context.Context) (cert string, certQuote []byte, err error)
	GetManifestSignature(context.Context) (manifestSignatureRootECDSA, manifestSignature, manifest []byte)
	GetSecrets(ctx context.Context, requestedSecrets []string, requestUser *user.User) (map[string]manifest.Secret, error)
	GetStatus(context.Context) (statusCode state.State, status string, err error)
	GetUpdateLog(context.Context) (updateLog string, err error)
	Recover(ctx context.Context, encryptionKey []byte) (int, error)
	VerifyUser(ctx context.Context, clientCerts []*x509.Certificate) (*user.User, error)
	UpdateManifest(ctx context.Context, rawUpdateManifest []byte, updater *user.User) error
	WriteSecrets(ctx context.Context, rawSecretManifest []byte, updater *user.User) error
}

// RunMarbleServer starts a gRPC with the given Coordinator core.
// `address` is the desired TCP address like "localhost:0".
// The effective TCP address is returned via `addrChan`.
func RunMarbleServer(core *core.Core, addr string, addrChan chan string, errChan chan error, zapLogger *zap.Logger, promRegistry *prometheus.Registry) {
	tlsConfig := tls.Config{
		GetCertificate: core.GetTLSMarbleRootCertificate,
		// NOTE: we'll verify the cert later using the given quote
		ClientAuth: tls.RequireAnyClientCert,
	}
	creds := credentials.NewTLS(&tlsConfig)

	// Make sure that log statements internal to gRPC library are logged using the zapLogger as well.
	grpc_zap.ReplaceGrpcLoggerV2(zapLogger)

	grpcMetrics := grpc_prometheus.NewServerMetrics()
	grpcServer := grpc.NewServer(
		grpc.Creds(creds),
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
			grpc_ctxtags.StreamServerInterceptor(),
			grpc_zap.StreamServerInterceptor(zapLogger),
			grpcMetrics.StreamServerInterceptor(),
		)),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			grpc_ctxtags.UnaryServerInterceptor(),
			grpc_zap.UnaryServerInterceptor(zapLogger),
			grpcMetrics.UnaryServerInterceptor(),
		)),
	)

	rpc.RegisterMarbleServer(grpcServer, core)
	if promRegistry != nil {
		grpcMetrics.InitializeMetrics(grpcServer)
		promRegistry.MustRegister(grpcMetrics)
	}
	socket, err := net.Listen("tcp", addr)
	if err != nil {
		errChan <- err
		return
	}
	addrChan <- socket.Addr().String()
	err = grpcServer.Serve(socket)
	if err != nil {
		errChan <- err
	}
}

// CreateServeMux creates a mux that serves the client API.
func CreateServeMux(api clientAPI, promFactory *promauto.Factory) serveMux {
	server := clientAPIServer{api}
	var router serveMux
	if promFactory != nil {
		muxRouter := newPromServeMux(promFactory, "server", "client_api")
		muxRouter.setMethodNotAllowedHandler(server.methodNotAllowedHandler)
		router = muxRouter
	} else {
		muxRouter := http.NewServeMux()
		muxRouter.HandleFunc("/", server.methodNotAllowedHandler)
		router = muxRouter
	}

	router.HandleFunc("/manifest", server.handleGetPost(server.manifestGet, server.manifestPost))
	router.HandleFunc("/update", server.handleGetPost(server.updateGet, server.updatePost))
	router.HandleFunc("/secrets", server.handleGetPost(server.secretsGet, server.secretsPost))
	router.HandleFunc("/status", server.handleGetPost(server.statusGet, server.methodNotAllowedHandler))
	router.HandleFunc("/quote", server.handleGetPost(server.quoteGet, server.methodNotAllowedHandler))
	router.HandleFunc("/recover", server.handleGetPost(server.methodNotAllowedHandler, server.recoverPost))
	return router
}

// RunClientServer runs a HTTP server serving mux.
func RunClientServer(mux http.Handler, address string, tlsConfig *tls.Config, zapLogger *zap.Logger) {
	server := http.Server{
		Addr:      address,
		Handler:   mux,
		TLSConfig: tlsConfig,
	}
	zapLogger.Info("Starting client https server", zap.String("address", address))
	err := server.ListenAndServeTLS("", "")
	zapLogger.Warn(err.Error())
}

// RunPrometheusServer runs a HTTP server handling the prometheus metrics endpoint.
func RunPrometheusServer(address string, zapLogger *zap.Logger, reg *prometheus.Registry, eventlog *events.Log) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.InstrumentMetricHandler(reg, promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})))
	mux.Handle("/events", eventlog.Handler())
	zapLogger.Info("Starting prometheus /metrics endpoint", zap.String("address", address))
	err := http.ListenAndServe(address, mux)
	zapLogger.Warn(err.Error())
}
