// Copyright 2018 Drone.IO Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v2"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"go.woodpecker-ci.org/woodpecker/v2/cmd/common"
	"go.woodpecker-ci.org/woodpecker/v2/pipeline/rpc/proto"
	"go.woodpecker-ci.org/woodpecker/v2/server"
	"go.woodpecker-ci.org/woodpecker/v2/server/cron"
	"go.woodpecker-ci.org/woodpecker/v2/server/forge"
	woodpeckerGrpcServer "go.woodpecker-ci.org/woodpecker/v2/server/grpc"
	"go.woodpecker-ci.org/woodpecker/v2/server/logging"
	"go.woodpecker-ci.org/woodpecker/v2/server/model"
	"go.woodpecker-ci.org/woodpecker/v2/server/plugins/config"
	"go.woodpecker-ci.org/woodpecker/v2/server/plugins/permissions"
	"go.woodpecker-ci.org/woodpecker/v2/server/pubsub"
	"go.woodpecker-ci.org/woodpecker/v2/server/router"
	"go.woodpecker-ci.org/woodpecker/v2/server/router/middleware"
	"go.woodpecker-ci.org/woodpecker/v2/server/store"
	"go.woodpecker-ci.org/woodpecker/v2/server/web"
	"go.woodpecker-ci.org/woodpecker/v2/shared/constant"
	"go.woodpecker-ci.org/woodpecker/v2/version"
	// "go.woodpecker-ci.org/woodpecker/v2/server/plugins/encryption"
	// encryptedStore "go.woodpecker-ci.org/woodpecker/v2/server/plugins/encryption/wrapper/store"
)

func run(c *cli.Context) error {
	common.SetupGlobalLogger(c, true)

	// set gin mode based on log level
	if zerolog.GlobalLevel() > zerolog.DebugLevel {
		gin.SetMode(gin.ReleaseMode)
	}

	if c.String("server-host") == "" {
		log.Fatal().Msg("WOODPECKER_HOST is not properly configured")
	}

	if !strings.Contains(c.String("server-host"), "://") {
		log.Fatal().Msg(
			"WOODPECKER_HOST must be <scheme>://<hostname> format",
		)
	}

	if _, err := url.Parse(c.String("server-host")); err != nil {
		log.Fatal().Err(err).Msg("could not parse WOODPECKER_HOST")
	}

	if strings.Contains(c.String("server-host"), "://localhost") {
		log.Warn().Msg(
			"WOODPECKER_HOST should probably be publicly accessible (not localhost)",
		)
	}

	_forge, err := setupForge(c)
	if err != nil {
		log.Fatal().Err(err).Msg("can't setup forge")
	}

	_store, err := setupStore(c)
	if err != nil {
		log.Fatal().Err(err).Msg("cant't setup database store")
	}
	defer func() {
		if err := _store.Close(); err != nil {
			log.Error().Err(err).Msg("could not close store")
		}
	}()

	setupEvilGlobals(c, _store, _forge)

	var g errgroup.Group

	setupMetrics(&g, _store)

	g.Go(func() error {
		return cron.Start(c.Context, _store, _forge)
	})

	// start the grpc server
	g.Go(func() error {
		lis, err := net.Listen("tcp", c.String("grpc-addr"))
		if err != nil {
			log.Fatal().Err(err).Msg("failed to listen on grpc-addr")
		}

		jwtSecret := c.String("grpc-secret")
		jwtManager := woodpeckerGrpcServer.NewJWTManager(jwtSecret)

		authorizer := woodpeckerGrpcServer.NewAuthorizer(jwtManager)
		grpcServer := grpc.NewServer(
			grpc.StreamInterceptor(authorizer.StreamInterceptor),
			grpc.UnaryInterceptor(authorizer.UnaryInterceptor),
			grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
				MinTime: c.Duration("keepalive-min-time"),
			}),
		)

		woodpeckerServer := woodpeckerGrpcServer.NewWoodpeckerServer(
			_forge,
			server.Config.Services.Queue,
			server.Config.Services.Logs,
			server.Config.Services.Pubsub,
			_store,
		)
		proto.RegisterWoodpeckerServer(grpcServer, woodpeckerServer)

		woodpeckerAuthServer := woodpeckerGrpcServer.NewWoodpeckerAuthServer(
			jwtManager,
			server.Config.Server.AgentToken,
			_store,
		)
		proto.RegisterWoodpeckerAuthServer(grpcServer, woodpeckerAuthServer)

		err = grpcServer.Serve(lis)
		if err != nil {
			log.Fatal().Err(err).Msg("failed to serve grpc server")
		}
		return nil
	})

	proxyWebUI := c.String("www-proxy")
	var webUIServe func(w http.ResponseWriter, r *http.Request)

	if proxyWebUI == "" {
		webEngine, err := web.New()
		if err != nil {
			log.Fatal().Err(err).Msg("failed to create web engine")
		}
		webUIServe = webEngine.ServeHTTP
	} else {
		origin, _ := url.Parse(proxyWebUI)

		director := func(req *http.Request) {
			req.Header.Add("X-Forwarded-Host", req.Host)
			req.Header.Add("X-Origin-Host", origin.Host)
			req.URL.Scheme = origin.Scheme
			req.URL.Host = origin.Host
		}

		proxy := &httputil.ReverseProxy{Director: director}
		webUIServe = proxy.ServeHTTP
	}

	// setup the server and start the listener
	handler := router.Load(
		webUIServe,
		middleware.Logger(time.RFC3339, true),
		middleware.Version,
		middleware.Store(c, _store),
	)

	if c.String("server-cert") != "" {
		// start the server with tls enabled
		g.Go(func() error {
			serve := &http.Server{
				Addr:    server.Config.Server.PortTLS,
				Handler: handler,
				TLSConfig: &tls.Config{
					NextProtos: []string{"h2", "http/1.1"},
				},
			}
			err = serve.ListenAndServeTLS(
				c.String("server-cert"),
				c.String("server-key"),
			)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal().Err(err).Msg("failed to start server with tls")
			}
			return err
		})

		// http to https redirect
		redirect := func(w http.ResponseWriter, req *http.Request) {
			serverURL, _ := url.Parse(server.Config.Server.Host)
			req.URL.Scheme = "https"
			req.URL.Host = serverURL.Host

			w.Header().Set("Strict-Transport-Security", "max-age=31536000")

			http.Redirect(w, req, req.URL.String(), http.StatusMovedPermanently)
		}

		g.Go(func() error {
			err := http.ListenAndServe(server.Config.Server.Port, http.HandlerFunc(redirect))
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal().Err(err).Msg("unable to start server to redirect from http to https")
			}
			return err
		})
	} else if c.Bool("lets-encrypt") {
		// start the server with lets-encrypt
		certmagic.DefaultACME.Email = c.String("lets-encrypt-email")
		certmagic.DefaultACME.Agreed = true

		address, err := url.Parse(strings.TrimSuffix(c.String("server-host"), "/"))
		if err != nil {
			return err
		}

		g.Go(func() error {
			if err := certmagic.HTTPS([]string{address.Host}, handler); err != nil {
				log.Fatal().Err(err).Msg("certmagic does not work")
			}
			return nil
		})
	} else {
		// start the server without tls
		g.Go(func() error {
			err := http.ListenAndServe(
				c.String("server-addr"),
				handler,
			)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal().Err(err).Msg("could not start server")
			}
			return err
		})
	}

	if metricsServerAddr := c.String("metrics-server-addr"); metricsServerAddr != "" {
		g.Go(func() error {
			metricsRouter := gin.New()
			metricsRouter.GET("/metrics", gin.WrapH(promhttp.Handler()))
			err := http.ListenAndServe(metricsServerAddr, metricsRouter)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal().Err(err).Msg("could not start metrics server")
			}
			return err
		})
	}

	log.Info().Msgf("Starting Woodpecker server with version '%s'", version.String())

	return g.Wait()
}

func setupEvilGlobals(c *cli.Context, v store.Store, f forge.Forge) {
	// forge
	server.Config.Services.Forge = f
	server.Config.Services.Timeout = c.Duration("forge-timeout")

	// services
	server.Config.Services.Queue = setupQueue(c, v)
	server.Config.Services.Logs = logging.New()
	server.Config.Services.Pubsub = pubsub.New()
	server.Config.Services.Registries = setupRegistryService(c, v)

	// TODO(1544): fix encrypted store
	// // encryption
	// encryptedSecretStore := encryptedStore.NewSecretStore(v)
	// err := encryption.Encryption(c, v).WithClient(encryptedSecretStore).Build()
	// if err != nil {
	// 	log.Fatal().Err(err).Msg("could not create encryption service")
	// }
	// server.Config.Services.Secrets = setupSecretService(c, encryptedSecretStore)
	server.Config.Services.Secrets = setupSecretService(c, v)

	server.Config.Services.Environ = setupEnvironService(c, v)
	server.Config.Services.Membership = setupMembershipService(c, f)

	server.Config.Services.SignaturePrivateKey, server.Config.Services.SignaturePublicKey = setupSignatureKeys(v)

	if endpoint := c.String("config-service-endpoint"); endpoint != "" {
		server.Config.Services.ConfigService = config.NewHTTP(endpoint, server.Config.Services.SignaturePrivateKey)
	}

	// authentication
	server.Config.Pipeline.AuthenticatePublicRepos = c.Bool("authenticate-public-repos")

	// Cloning
	server.Config.Pipeline.DefaultCloneImage = c.String("default-clone-image")
	constant.TrustedCloneImages = append(constant.TrustedCloneImages, server.Config.Pipeline.DefaultCloneImage)

	// Execution
	_events := c.StringSlice("default-cancel-previous-pipeline-events")
	events := make([]model.WebhookEvent, len(_events))
	for _, v := range _events {
		events = append(events, model.WebhookEvent(v))
	}
	server.Config.Pipeline.DefaultCancelPreviousPipelineEvents = events
	server.Config.Pipeline.DefaultTimeout = c.Int64("default-pipeline-timeout")
	server.Config.Pipeline.MaxTimeout = c.Int64("max-pipeline-timeout")

	// limits
	server.Config.Pipeline.Limits.MemSwapLimit = c.Int64("limit-mem-swap")
	server.Config.Pipeline.Limits.MemLimit = c.Int64("limit-mem")
	server.Config.Pipeline.Limits.ShmSize = c.Int64("limit-shm-size")
	server.Config.Pipeline.Limits.CPUQuota = c.Int64("limit-cpu-quota")
	server.Config.Pipeline.Limits.CPUShares = c.Int64("limit-cpu-shares")
	server.Config.Pipeline.Limits.CPUSet = c.String("limit-cpu-set")

	// backend options for pipeline compiler
	server.Config.Pipeline.Proxy.No = c.String("backend-no-proxy")
	server.Config.Pipeline.Proxy.HTTP = c.String("backend-http-proxy")
	server.Config.Pipeline.Proxy.HTTPS = c.String("backend-https-proxy")

	// server configuration
	server.Config.Server.Cert = c.String("server-cert")
	server.Config.Server.Key = c.String("server-key")
	server.Config.Server.AgentToken = c.String("agent-secret")
	serverHost := strings.TrimSuffix(c.String("server-host"), "/")
	server.Config.Server.Host = serverHost
	if c.IsSet("server-webhook-host") {
		server.Config.Server.WebhookHost = c.String("server-webhook-host")
	} else {
		server.Config.Server.WebhookHost = serverHost
	}
	if c.IsSet("server-dev-oauth-host") {
		server.Config.Server.OAuthHost = c.String("server-dev-oauth-host")
	} else {
		server.Config.Server.OAuthHost = serverHost
	}
	server.Config.Server.Port = c.String("server-addr")
	server.Config.Server.PortTLS = c.String("server-addr-tls")
	server.Config.Server.StatusContext = c.String("status-context")
	server.Config.Server.StatusContextFormat = c.String("status-context-format")
	server.Config.Server.SessionExpires = c.Duration("session-expires")
	rootPath := c.String("root-path")
	if !c.IsSet("root-path") {
		// Extract RootPath from Host...
		u, _ := url.Parse(server.Config.Server.Host)
		rootPath = u.Path
	}
	rootPath = strings.TrimSuffix(rootPath, "/")
	if rootPath != "" && !strings.HasPrefix(rootPath, "/") {
		rootPath = "/" + rootPath
	}
	server.Config.Server.RootPath = rootPath
	server.Config.Server.CustomCSSFile = strings.TrimSpace(c.String("custom-css-file"))
	server.Config.Server.CustomJsFile = strings.TrimSpace(c.String("custom-js-file"))
	server.Config.Pipeline.Networks = c.StringSlice("network")
	server.Config.Pipeline.Volumes = c.StringSlice("volume")
	server.Config.Pipeline.Privileged = c.StringSlice("escalate")
	server.Config.Server.EnableSwagger = c.Bool("enable-swagger")

	// prometheus
	server.Config.Prometheus.AuthToken = c.String("prometheus-auth-token")

	// permissions
	server.Config.Permissions.Open = c.Bool("open")
	server.Config.Permissions.Admins = permissions.NewAdmins(c.StringSlice("admin"))
	server.Config.Permissions.Orgs = permissions.NewOrgs(c.StringSlice("orgs"))
	server.Config.Permissions.OwnersAllowlist = permissions.NewOwnersAllowlist(c.StringSlice("repo-owners"))
}
