// Copyright 2020 The PipeCD Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/kapetaniosci/pipe/pkg/datastore"
	"github.com/kapetaniosci/pipe/pkg/datastore/firestore"

	jwtgo "github.com/dgrijalva/jwt-go"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/kapetaniosci/pipe/pkg/admin"
	"github.com/kapetaniosci/pipe/pkg/app/api/api"
	"github.com/kapetaniosci/pipe/pkg/app/api/service/webservice"
	"github.com/kapetaniosci/pipe/pkg/cli"
	"github.com/kapetaniosci/pipe/pkg/jwt"
	"github.com/kapetaniosci/pipe/pkg/rpc"
)

var (
	defaultSigningMethod = jwtgo.SigningMethodHS256
)

type httpHandler interface {
	Register(func(pattern string, handler func(http.ResponseWriter, *http.Request)))
}

type server struct {
	pipedAPIPort int
	webAPIPort   int
	webhookPort  int
	adminPort    int
	gracePeriod  time.Duration

	tls                 bool
	certFile            string
	keyFile             string
	tokenSigningKeyFile string

	datastoreVariant         string
	datastoreType            string
	datastoreCredentialsFile string
	gcpProjectID             string
}

func NewCommand() *cobra.Command {
	s := &server{
		pipedAPIPort: 9080,
		webAPIPort:   9081,
		webhookPort:  9082,
		adminPort:    9085,
		gracePeriod:  15 * time.Second,
	}
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Start running API server.",
		RunE:  cli.WithContext(s.run),
	}

	cmd.Flags().IntVar(&s.pipedAPIPort, "piped-api-port", s.pipedAPIPort, "The port number used to run a grpc server that serving serves incoming piped requests.")
	cmd.Flags().IntVar(&s.webAPIPort, "web-api-port", s.webAPIPort, "The port number used to run a grpc server that serves incoming web requests.")
	cmd.Flags().IntVar(&s.webhookPort, "webhook-port", s.webhookPort, "The port number used to run a http server that serves incoming webhook events.")
	cmd.Flags().IntVar(&s.adminPort, "admin-port", s.adminPort, "The port number used to run a HTTP server for admin tasks such as metrics, healthz.")
	cmd.Flags().DurationVar(&s.gracePeriod, "grace-period", s.gracePeriod, "How long to wait for graceful shutdown.")

	cmd.Flags().BoolVar(&s.tls, "tls", s.tls, "Whether running the gRPC server with TLS or not.")
	cmd.Flags().StringVar(&s.certFile, "cert-file", s.certFile, "The path to the TLS certificate file.")
	cmd.Flags().StringVar(&s.keyFile, "key-file", s.keyFile, "The path to the TLS key file.")
	cmd.Flags().StringVar(&s.tokenSigningKeyFile, "token-signing-key-file", s.tokenSigningKeyFile, "The path to key file used to sign ID token.")

	cmd.Flags().StringVar(&s.datastoreVariant, "datastore-variant", s.datastoreVariant, "The identifier that logically separates environment of the datastore.")
	cmd.Flags().StringVar(&s.datastoreType, "datastore-type", s.datastoreType, "The type of datastore which persist piped data.")
	cmd.Flags().StringVar(&s.datastoreCredentialsFile, "datastore-credentials-file", s.datastoreCredentialsFile, "The path to the credentials file for accessing datastore.")
	cmd.Flags().StringVar(&s.gcpProjectID, "gcp-project-id", s.gcpProjectID, "The identifier of the GCP project which host the control plane.")

	cmd.MarkFlagRequired("datastore-variant")
	cmd.MarkFlagRequired("datastore-type")

	return cmd
}

func (s *server) run(ctx context.Context, t cli.Telemetry) error {
	group, ctx := errgroup.WithContext(ctx)

	// signer, err := jwt.NewSigner(defaultSigningMethod, s.tokenSigningKeyFile)
	// if err != nil {
	// 	t.Logger.Error("failed to create a new signer", zap.Error(err))
	// 	return err
	// }
	verifier, err := jwt.NewVerifier(defaultSigningMethod, s.tokenSigningKeyFile)
	if err != nil {
		t.Logger.Error("failed to create a new verifier", zap.Error(err))
		return err
	}

	var (
		pipedAPIServer *rpc.Server
		webAPIServer   *rpc.Server
	)

	// Start a gRPC server for handling PipedAPI requests.
	{
		ds, err := s.createDatastore(ctx, t.Logger)
		if err != nil {
			t.Logger.Error("failed creating datastore", zap.Error(err))
			return err
		}
		defer func() {
			if err := ds.Close(); err != nil {
				t.Logger.Error("failed closing datastore client", zap.Error(err))
			}
		}()
		service := api.NewPipedAPI(ds, t.Logger)
		opts := []rpc.Option{
			rpc.WithPort(s.pipedAPIPort),
			rpc.WithGracePeriod(s.gracePeriod),
			rpc.WithLogger(t.Logger),
			// rpc.WithPipedTokenAuthUnaryInterceptor(verifier, t.Logger),
			rpc.WithRequestValidationUnaryInterceptor(),
		}
		if s.tls {
			opts = append(opts, rpc.WithTLS(s.certFile, s.keyFile))
		}
		pipedAPIServer = rpc.NewServer(service, opts...)
		group.Go(func() error {
			return pipedAPIServer.Run(ctx)
		})
	}

	// Start a gRPC server for handling WebAPI requests.
	{
		service := api.NewWebAPI(t.Logger)
		opts := []rpc.Option{
			rpc.WithPort(s.webAPIPort),
			rpc.WithGracePeriod(s.gracePeriod),
			rpc.WithLogger(t.Logger),
			rpc.WithJWTAuthUnaryInterceptor(verifier, webservice.NewRBACAuthorizer(), t.Logger),
			rpc.WithRequestValidationUnaryInterceptor(),
		}
		if s.tls {
			opts = append(opts, rpc.WithTLS(s.certFile, s.keyFile))
		}
		webAPIServer = rpc.NewServer(service, opts...)
		group.Go(func() error {
			return webAPIServer.Run(ctx)
		})
	}

	// Start an http server for handling incoming webhook events.
	{
		mux := http.NewServeMux()
		httpServer := &http.Server{
			Addr:    fmt.Sprintf(":%d", s.webhookPort),
			Handler: mux,
		}
		handlers := []httpHandler{
			//authhandler.NewHandler(signer, verifier, config, ac, t.Logger),
		}
		for _, h := range handlers {
			h.Register(mux.HandleFunc)
		}
		group.Go(func() error {
			return runHttpServer(ctx, httpServer, s.gracePeriod, t.Logger)
		})
	}

	// Start running admin server.
	{
		admin := admin.NewAdmin(s.adminPort, s.gracePeriod, t.Logger)
		if exporter, ok := t.PrometheusMetricsExporter(); ok {
			admin.Handle("/metrics", exporter)
		}
		admin.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("ok"))
		})
		group.Go(func() error {
			return admin.Run(ctx)
		})
	}

	// Wait until all components have finished.
	// A terminating signal or a finish of any components
	// could trigger the finish of server.
	// This ensures that all components are good or no one.
	if err := group.Wait(); err != nil {
		t.Logger.Error("failed while running", zap.Error(err))
		return err
	}
	return nil
}

func runHttpServer(ctx context.Context, httpServer *http.Server, gracePeriod time.Duration, logger *zap.Logger) error {
	doneCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		defer cancel()
		logger.Info("start running http server")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("failed to listen and http server", zap.Error(err))
			doneCh <- err
		}
		doneCh <- nil
	}()

	<-ctx.Done()

	ctx, _ = context.WithTimeout(context.Background(), gracePeriod)
	logger.Info("stopping http server")
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Error("failed to shutdown http server", zap.Error(err))
	}

	return <-doneCh
}

func (s *server) createDatastore(ctx context.Context, logger *zap.Logger) (datastore.DataStore, error) {
	switch s.datastoreType {
	case "firestore":
		if s.datastoreVariant == "" {
			return nil, fmt.Errorf("datastore: datastore-variant is required for %s", s.datastoreType)
		}
		if s.datastoreCredentialsFile == "" {
			return nil, fmt.Errorf("datastore: datastore-credentials-file is required for %s", s.datastoreType)
		}
		if s.gcpProjectID == "" {
			return nil, fmt.Errorf("datastore: gcp-project-id is required for %s", s.datastoreType)
		}
		options := []firestore.Option{
			firestore.WithCredentialsFile(s.datastoreCredentialsFile),
			firestore.WithLogger(logger),
		}
		return firestore.NewFireStore(ctx, s.gcpProjectID, s.datastoreVariant, options...)
	default:
		return nil, fmt.Errorf("invalid datastore type: %s", s.datastoreType)
	}
}
