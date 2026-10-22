// SPDX-License-Identifier:Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	stdlog "log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/metallb/frr-k8s/cmd/metrics/collector"
	"github.com/metallb/frr-k8s/cmd/metrics/liveness"
	"github.com/metallb/frr-k8s/cmd/metrics/vtysh"
	"github.com/metallb/frr-k8s/internal/logging"
	"github.com/metallb/frr-k8s/internal/tlsconfig"
	"github.com/metallb/frr-k8s/internal/version"
)

var (
	metricsPort         = flag.Uint("metrics-port", 9141, "Port to listen on for web interface.")
	metricsBindAddress  = flag.String("metrics-bind-address", "0.0.0.0", "The address the metric endpoint binds to")
	metricsPath         = flag.String("metrics-path", "/metrics", "Path under which to expose metrics.")
	disableMetricsAuth  = flag.Bool("disable-metrics-auth", false, "Disable Kubernetes authentication and authorization for metrics. TLS remains enabled; restrict access to the endpoint.")
	tlsCertFile         = flag.String("tls-cert-file", "", "x509 certificate for HTTPS. If both certificate and key are empty, a self-signed cert is auto-generated.")
	tlsKeyFile          = flag.String("tls-private-key-file", "", "x509 private key matching --tls-cert-file.")
	tlsCipherSuites     = flag.String("tls-cipher-suites", "", "Comma-separated list of TLS cipher suites. If empty, uses Go defaults.")
	tlsCurvePreferences = flag.String("tls-curve-preferences", "", "Comma-separated list of numeric CurveID values (see https://pkg.go.dev/crypto/tls#CurveID). If empty, uses Go defaults.")
	tlsMinVersionFlag   = flag.String("tls-min-version", "", "Minimum TLS version (VersionTLS12 or VersionTLS13). If empty, defaults to VersionTLS13.")
)

func main() {
	flag.Parse()

	if err := logging.Init(); err != nil {
		fmt.Printf("failed to initialize logging: %s\n", err)
		os.Exit(1)
	}
	logger := logging.GetLogger()
	logger.SetLogLevel(logging.LevelError)
	if *disableMetricsAuth {
		// Keep the explicit security-mode notice visible at the default error log level.
		_ = logger.Log("msg", "Kubernetes metrics authentication and authorization disabled; restrict access to the endpoint", "metrics_auth", "disabled")
	}

	level.Info(logger).Log("version", version.Version(), "commit", version.CommitHash(), "branch", version.Branch(), "goversion", version.GoString(), "msg", "FRR metrics exporter starting "+version.String())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	err := run(ctx)
	stop()
	if err != nil {
		level.Error(logger).Log("msg", "metrics exporter stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	logger := logging.GetLogger()
	mux := http.NewServeMux()
	metricsHandler, err := newMetricsHandler(*disableMetricsAuth, rbacFilter)
	if err != nil {
		return fmt.Errorf("creating metrics handler: %w", err)
	}
	mux.Handle(*metricsPath, metricsHandler)
	mux.Handle("/livez", liveness.Handler(vtysh.Run, logger))

	tlsOpt, err := tlsconfig.OptFor(*tlsCipherSuites, *tlsCurvePreferences, *tlsMinVersionFlag)
	if err != nil {
		return fmt.Errorf("parsing TLS flags: %w", err)
	}
	watchCtx, cancelWatch := context.WithCancel(ctx)
	tlsCfg, watchResult, err := tlsconfig.For(watchCtx, tlsOpt, *tlsCertFile, *tlsKeyFile)
	defer func() {
		cancelWatch()
		if watchResult != nil {
			<-watchResult
		}
	}()
	if err != nil {
		return fmt.Errorf("configuring TLS: %w", err)
	}

	srv := &http.Server{
		Addr:        fmt.Sprintf("%s:%d", *metricsBindAddress, *metricsPort),
		ReadTimeout: 3 * time.Second,
		Handler:     mux,
		TLSConfig:   tlsCfg,
	}

	level.Info(logger).Log("msg", "Starting exporter", "metricsPath", *metricsPath, "port", *metricsPort)

	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("listening for metrics: %w", err)
	}
	return serve(ctx, srv, listener, watchResult)
}

// serve waits for HTTP drain or forced connection close before returning. Existing CLI
// queries still need their own cancellation and deadlines; closing a connection
// cannot stop a handler blocked in a subprocess.
func serve(ctx context.Context, srv *http.Server, listener net.Listener, watchResult <-chan error) error {
	serverResult := make(chan error, 1)
	go func() {
		serverResult <- srv.ServeTLS(listener, "", "")
	}()

	var serveErr error
	serverStopped := false
	select {
	case err := <-serverResult:
		serverStopped = true
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case <-ctx.Done():
	case err := <-watchResult:
		if ctx.Err() == nil {
			if err == nil {
				err = errors.New("watcher stopped unexpectedly")
			}
			serveErr = fmt.Errorf("watching TLS certificate: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		serveErr = errors.Join(serveErr, fmt.Errorf("shutting down metrics server: %w", err), srv.Close())
	}
	if !serverStopped {
		if err := <-serverResult; !errors.Is(err, http.ErrServerClosed) {
			serveErr = errors.Join(serveErr, err)
		}
	}
	return serveErr
}

func newMetricsHandler(disableAuth bool, filterFactory func() (metricsserver.Filter, error)) (http.Handler, error) {
	handler := promHandler()
	if disableAuth {
		return handler, nil
	}
	filter, err := filterFactory()
	if err != nil {
		return nil, err
	}
	return filter(ctrl.Log.WithName("metrics-auth"), handler)
}

func promHandler() http.Handler {
	logger := logging.GetLogger()
	BGPCollector := collector.NewBGP(logger)
	BFDCollector := collector.NewBFD(logger)

	registry := prometheus.NewRegistry()
	registry.MustRegister(BGPCollector)
	registry.MustRegister(BFDCollector)

	return promhttp.HandlerFor(
		prometheus.Gatherers{prometheus.DefaultGatherer, registry},
		promhttp.HandlerOpts{
			ErrorLog:      stdlog.New(log.NewStdlibAdapter(level.Error(logger)), "", 0),
			ErrorHandling: promhttp.ContinueOnError,
			Registry:      registry,
		},
	)
}

func rbacFilter() (metricsserver.Filter, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("getting in-cluster config: %w", err)
	}
	httpClient, err := rest.HTTPClientFor(config)
	if err != nil {
		return nil, fmt.Errorf("creating HTTP client: %w", err)
	}
	return filters.WithAuthenticationAndAuthorization(config, httpClient)
}
