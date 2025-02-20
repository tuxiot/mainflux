// Copyright (c) Mainflux
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	kitprometheus "github.com/go-kit/kit/metrics/prometheus"
	"github.com/jmoiron/sqlx"
	"github.com/mainflux/mainflux"
	authapi "github.com/mainflux/mainflux/auth/api/grpc"
	"github.com/mainflux/mainflux/consumers"
	"github.com/mainflux/mainflux/consumers/notifiers"
	"github.com/mainflux/mainflux/consumers/notifiers/api"
	"github.com/mainflux/mainflux/consumers/notifiers/postgres"

	mfsmpp "github.com/mainflux/mainflux/consumers/notifiers/smpp"
	"github.com/mainflux/mainflux/consumers/notifiers/tracing"
	"github.com/mainflux/mainflux/logger"
	"github.com/mainflux/mainflux/pkg/messaging/nats"
	"github.com/mainflux/mainflux/pkg/ulid"
	opentracing "github.com/opentracing/opentracing-go"
	stdprometheus "github.com/prometheus/client_golang/prometheus"
	jconfig "github.com/uber/jaeger-client-go/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	defLogLevel      = "error"
	defDBHost        = "localhost"
	defDBPort        = "5432"
	defDBUser        = "mainflux"
	defDBPass        = "mainflux"
	defDB            = "subscriptions"
	defConfigPath    = "/config.toml"
	defDBSSLMode     = "disable"
	defDBSSLCert     = ""
	defDBSSLKey      = ""
	defDBSSLRootCert = ""
	defHTTPPort      = "8907"
	defServerCert    = ""
	defServerKey     = ""
	defFrom          = ""
	defJaegerURL     = ""
	defNatsURL       = "nats://localhost:4222"

	defSmppAddress    = ""
	defSmppUsername   = ""
	defSmppPassword   = ""
	defSmppSystemType = ""
	defSmppSrcAddrTON = "0"
	defSmppDstAddrTON = "0"
	defSmppSrcAddrNPI = "0"
	defSmppDstAddrNPI = "0"

	defAuthTLS     = "false"
	defAuthCACerts = ""
	defAuthURL     = "localhost:8181"
	defAuthTimeout = "1s"

	envLogLevel      = "MF_SMPP_NOTIFIER_LOG_LEVEL"
	envDBHost        = "MF_SMPP_NOTIFIER_DB_HOST"
	envDBPort        = "MF_SMPP_NOTIFIER_DB_PORT"
	envDBUser        = "MF_SMPP_NOTIFIER_DB_USER"
	envDBPass        = "MF_SMPP_NOTIFIER_DB_PASS"
	envDB            = "MF_SMPP_NOTIFIER_DB"
	envConfigPath    = "MF_SMPP_NOTIFIER_WRITER_CONFIG_PATH"
	envDBSSLMode     = "MF_SMPP_NOTIFIER_DB_SSL_MODE"
	envDBSSLCert     = "MF_SMPP_NOTIFIER_DB_SSL_CERT"
	envDBSSLKey      = "MF_SMPP_NOTIFIER_DB_SSL_KEY"
	envDBSSLRootCert = "MF_SMPP_NOTIFIER_DB_SSL_ROOT_CERT"
	envHTTPPort      = "MF_SMPP_NOTIFIER_HTTP_PORT"
	envServerCert    = "MF_SMPP_NOTIFIER_SERVER_CERT"
	envServerKey     = "MF_SMPP_NOTIFIER_SERVER_KEY"
	envFrom          = "MF_SMPP_NOTIFIER_SOURCE_ADDR"
	envJaegerURL     = "MF_JAEGER_URL"
	envNatsURL       = "MF_NATS_URL"

	envSmppAddress    = "MF_SMPP_ADDRESS"
	envSmppUsername   = "MF_SMPP_USERNAME"
	envSmppPassword   = "MF_SMPP_PASSWORD"
	envSmppSystemType = "MF_SMPP_SYSTEM_TYPE"
	envSmppSrcAddrTON = "MF_SMPP_SRC_ADDR_TON"
	envSmppDstAddrTON = "MF_SMPP_DST_ADDR_TON"
	envSmppSrcAddrNPI = "MF_SMPP_SRC_ADDR_NPI"
	envSmppDstAddrNPI = "MF_SMPP_DST_ADDR_NPI"

	envAuthTLS     = "MF_AUTH_CLIENT_TLS"
	envAuthCACerts = "MF_AUTH_CA_CERTS"
	envAuthURL     = "MF_AUTH_GRPC_URL"
	envAuthTimeout = "MF_AUTH_GRPC_TIMEOUT"
)

type config struct {
	natsURL     string
	configPath  string
	logLevel    string
	dbConfig    postgres.Config
	smppConf    mfsmpp.Config
	from        string
	httpPort    string
	serverCert  string
	serverKey   string
	jaegerURL   string
	authTLS     bool
	authCACerts string
	authURL     string
	authTimeout time.Duration
}

func main() {
	cfg := loadConfig()

	logger, err := logger.New(os.Stdout, cfg.logLevel)
	if err != nil {
		log.Fatalf(err.Error())
	}

	db := connectToDB(cfg.dbConfig, logger)
	defer db.Close()

	pubSub, err := nats.NewPubSub(cfg.natsURL, "", logger)
	if err != nil {
		logger.Error(fmt.Sprintf("Failed to connect to NATS: %s", err))
		os.Exit(1)
	}
	defer pubSub.Close()

	authTracer, closer := initJaeger("auth", cfg.jaegerURL, logger)
	defer closer.Close()

	auth, close := connectToAuth(cfg, authTracer, logger)
	if close != nil {
		defer close()
	}

	tracer, closer := initJaeger("smpp-notifier", cfg.jaegerURL, logger)
	defer closer.Close()

	dbTracer, dbCloser := initJaeger("smpp-notifier_db", cfg.jaegerURL, logger)
	defer dbCloser.Close()

	svc := newService(db, dbTracer, auth, cfg, logger)
	errs := make(chan error, 2)

	if err = consumers.Start(pubSub, svc, nil, cfg.configPath, logger); err != nil {
		logger.Error(fmt.Sprintf("Failed to create Postgres writer: %s", err))
	}

	go startHTTPServer(tracer, svc, cfg.httpPort, cfg.serverCert, cfg.serverKey, logger, errs)

	go func() {
		c := make(chan os.Signal)
		signal.Notify(c, syscall.SIGINT)
		errs <- fmt.Errorf("%s", <-c)
	}()

	err = <-errs
	logger.Error(fmt.Sprintf("Users service terminated: %s", err))
}

func loadConfig() config {
	authTimeout, err := time.ParseDuration(mainflux.Env(envAuthTimeout, defAuthTimeout))
	if err != nil {
		log.Fatalf("Invalid %s value: %s", envAuthTimeout, err.Error())
	}

	tls, err := strconv.ParseBool(mainflux.Env(envAuthTLS, defAuthTLS))
	if err != nil {
		log.Fatalf("Invalid value passed for %s\n", envAuthTLS)
	}

	dbConfig := postgres.Config{
		Host:        mainflux.Env(envDBHost, defDBHost),
		Port:        mainflux.Env(envDBPort, defDBPort),
		User:        mainflux.Env(envDBUser, defDBUser),
		Pass:        mainflux.Env(envDBPass, defDBPass),
		Name:        mainflux.Env(envDB, defDB),
		SSLMode:     mainflux.Env(envDBSSLMode, defDBSSLMode),
		SSLCert:     mainflux.Env(envDBSSLCert, defDBSSLCert),
		SSLKey:      mainflux.Env(envDBSSLKey, defDBSSLKey),
		SSLRootCert: mainflux.Env(envDBSSLRootCert, defDBSSLRootCert),
	}

	saton, err := strconv.ParseUint(mainflux.Env(envSmppSrcAddrTON, defSmppSrcAddrTON), 10, 8)
	if err != nil {
		log.Fatalf("Invalid value passed for %s", envSmppSrcAddrTON)
	}
	daton, err := strconv.ParseUint(mainflux.Env(envSmppDstAddrTON, defSmppDstAddrTON), 10, 8)
	if err != nil {
		log.Fatalf("Invalid value passed for %s", envSmppDstAddrTON)
	}
	sanpi, err := strconv.ParseUint(mainflux.Env(envSmppSrcAddrNPI, defSmppSrcAddrNPI), 10, 8)
	if err != nil {
		log.Fatalf("Invalid value passed for %s", envSmppSrcAddrNPI)
	}
	danpi, err := strconv.ParseUint(mainflux.Env(envSmppDstAddrNPI, defSmppDstAddrNPI), 10, 8)
	if err != nil {
		log.Fatalf("Invalid value passed for %s", envSmppDstAddrNPI)
	}

	smppConf := mfsmpp.Config{
		Address:       mainflux.Env(envSmppAddress, defSmppAddress),
		Username:      mainflux.Env(envSmppUsername, defSmppUsername),
		Password:      mainflux.Env(envSmppPassword, defSmppPassword),
		SystemType:    mainflux.Env(envSmppSystemType, defSmppSystemType),
		SourceAddrTON: uint8(saton),
		DestAddrTON:   uint8(daton),
		SourceAddrNPI: uint8(sanpi),
		DestAddrNPI:   uint8(danpi),
	}

	return config{
		logLevel:    mainflux.Env(envLogLevel, defLogLevel),
		natsURL:     mainflux.Env(envNatsURL, defNatsURL),
		configPath:  mainflux.Env(envConfigPath, defConfigPath),
		dbConfig:    dbConfig,
		smppConf:    smppConf,
		from:        mainflux.Env(envFrom, defFrom),
		httpPort:    mainflux.Env(envHTTPPort, defHTTPPort),
		serverCert:  mainflux.Env(envServerCert, defServerCert),
		serverKey:   mainflux.Env(envServerKey, defServerKey),
		jaegerURL:   mainflux.Env(envJaegerURL, defJaegerURL),
		authTLS:     tls,
		authCACerts: mainflux.Env(envAuthCACerts, defAuthCACerts),
		authURL:     mainflux.Env(envAuthURL, defAuthURL),
		authTimeout: authTimeout,
	}

}

func initJaeger(svcName, url string, logger logger.Logger) (opentracing.Tracer, io.Closer) {
	if url == "" {
		return opentracing.NoopTracer{}, ioutil.NopCloser(nil)
	}

	tracer, closer, err := jconfig.Configuration{
		ServiceName: svcName,
		Sampler: &jconfig.SamplerConfig{
			Type:  "const",
			Param: 1,
		},
		Reporter: &jconfig.ReporterConfig{
			LocalAgentHostPort: url,
			LogSpans:           true,
		},
	}.NewTracer()
	if err != nil {
		logger.Error(fmt.Sprintf("Failed to init Jaeger: %s", err))
		os.Exit(1)
	}

	return tracer, closer
}

func connectToDB(dbConfig postgres.Config, logger logger.Logger) *sqlx.DB {
	db, err := postgres.Connect(dbConfig)
	if err != nil {
		logger.Error(fmt.Sprintf("Failed to connect to postgres: %s", err))
		os.Exit(1)
	}
	return db
}

func connectToAuth(cfg config, tracer opentracing.Tracer, logger logger.Logger) (mainflux.AuthServiceClient, func() error) {
	var opts []grpc.DialOption
	if cfg.authTLS {
		if cfg.authCACerts != "" {
			tpc, err := credentials.NewClientTLSFromFile(cfg.authCACerts, "")
			if err != nil {
				logger.Error(fmt.Sprintf("Failed to create tls credentials: %s", err))
				os.Exit(1)
			}
			opts = append(opts, grpc.WithTransportCredentials(tpc))
		}
	} else {
		opts = append(opts, grpc.WithInsecure())
		logger.Info("gRPC communication is not encrypted")
	}

	conn, err := grpc.Dial(cfg.authURL, opts...)
	if err != nil {
		logger.Error(fmt.Sprintf("Failed to connect to auth service: %s", err))
		os.Exit(1)
	}

	return authapi.NewClient(tracer, conn, cfg.authTimeout), conn.Close
}

func newService(db *sqlx.DB, tracer opentracing.Tracer, auth mainflux.AuthServiceClient, c config, logger logger.Logger) notifiers.Service {
	database := postgres.NewDatabase(db)
	repo := tracing.New(postgres.New(database), tracer)
	idp := ulid.New()
	notifier := mfsmpp.New(c.smppConf)
	svc := notifiers.New(auth, repo, idp, notifier, c.from)
	svc = api.LoggingMiddleware(svc, logger)
	svc = api.MetricsMiddleware(
		svc,
		kitprometheus.NewCounterFrom(stdprometheus.CounterOpts{
			Namespace: "notifier",
			Subsystem: "smpp",
			Name:      "request_count",
			Help:      "Number of requests received.",
		}, []string{"method"}),
		kitprometheus.NewSummaryFrom(stdprometheus.SummaryOpts{
			Namespace: "notifier",
			Subsystem: "smpp",
			Name:      "request_latency_microseconds",
			Help:      "Total duration of requests in microseconds.",
		}, []string{"method"}),
	)
	return svc
}

func startHTTPServer(tracer opentracing.Tracer, svc notifiers.Service, port string, certFile string, keyFile string, logger logger.Logger, errs chan error) {
	p := fmt.Sprintf(":%s", port)
	if certFile != "" || keyFile != "" {
		logger.Info(fmt.Sprintf("SMPP notifier service started using https, cert %s key %s, exposed port %s", certFile, keyFile, port))
		errs <- http.ListenAndServeTLS(p, certFile, keyFile, api.MakeHandler(svc, tracer))
	} else {
		logger.Info(fmt.Sprintf("SMPP notifier service started using http, exposed port %s", port))
		errs <- http.ListenAndServe(p, api.MakeHandler(svc, tracer))
	}
}
