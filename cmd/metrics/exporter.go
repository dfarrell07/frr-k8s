// SPDX-License-Identifier:Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
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
	collectorNames      = flag.String("collectors", "bgp,bfd", "Comma-separated collectors: bgp,bfd.")
	scrapeTimeout       = flag.Duration("scrape-timeout", 8*time.Second, "Deadline for all FRR reads per scrape; keep below the scraper timeout.")
	metricsPort         = flag.Uint("metrics-port", 9141, "Port to listen on for web interface.")
	metricsBindAddress  = flag.String("metrics-bind-address", "0.0.0.0", "The address the metric endpoint binds to")
	metricsPath         = flag.String("metrics-path", "/metrics", "Path under which to expose metrics.")
	disableMetricsAuth  = flag.Bool("disable-metrics-auth", false, "Disable Kubernetes authentication and authorization. Anyone who can reach the listener can read metrics.")
	tlsCertFile         = flag.String("tls-cert-file", "", "x509 certificate for HTTPS. If empty, a self-signed cert is auto-generated.")
	tlsKeyFile          = flag.String("tls-private-key-file", "", "x509 private key matching --tls-cert-file.")
	tlsCipherSuites     = flag.String("tls-cipher-suites", "", "Comma-separated list of TLS cipher suites. If empty, uses Go defaults.")
	tlsCurvePreferences = flag.String("tls-curve-preferences", "", "Comma-separated list of numeric CurveID values (see https://pkg.go.dev/crypto/tls#CurveID). If empty, uses Go defaults.")
	tlsMinVersionFlag   = flag.String("tls-min-version", "", "Minimum TLS version (VersionTLS12 or VersionTLS13). If empty, defaults to VersionTLS13.")
)

func main() {
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := logging.Init(); err != nil {
		fmt.Printf("failed to initialize logging: %s\n", err)
		os.Exit(1)
	}
	logger := logging.GetLogger()
	logger.SetLogLevel(logging.LevelError)

	level.Info(logger).Log("version", version.Version(), "commit", version.CommitHash(), "branch", version.Branch(), "goversion", version.GoString(), "msg", "FRR metrics exporter starting "+version.String())

	mux := http.NewServeMux()
	metricsHandler, err := newMetricsHandler(*disableMetricsAuth)
	if err != nil {
		level.Error(logger).Log("msg", "failed to create metrics handler", "error", err)
		os.Exit(1)
	}
	mux.Handle(*metricsPath, metricsHandler)
	mux.Handle("/livez", liveness.Handler((vtysh.Vtysh{}).Run, logger))

	tlsOpt, err := tlsconfig.OptFor(*tlsCipherSuites, *tlsCurvePreferences, *tlsMinVersionFlag)
	if err != nil {
		level.Error(logger).Log("msg", "failed to parse TLS flags", "error", err)
		os.Exit(1)
	}
	tlsCfg, err := tlsconfig.ForContext(ctx, tlsOpt, *tlsCertFile, *tlsKeyFile)
	if err != nil {
		level.Error(logger).Log("msg", "failed to configure TLS", "error", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:         net.JoinHostPort(*metricsBindAddress, fmt.Sprint(*metricsPort)),
		ReadTimeout:  3 * time.Second,
		WriteTimeout: *scrapeTimeout + 5*time.Second,
		IdleTimeout:  2 * time.Minute,
		Handler:      mux,
		TLSConfig:    tlsCfg,
	}

	level.Info(logger).Log("msg", "Starting exporter", "metricsPath", *metricsPath, "port", *metricsPort, "metricsAuthDisabled", *disableMetricsAuth)

	if err := serve(ctx, srv, 5*time.Second); err != nil {
		level.Error(logger).Log("error", err)
		os.Exit(1)
	}
}

func newMetricsHandler(disableAuth bool) (*exporter, error) {
	handler, err := promHandler()
	if err != nil {
		return nil, err
	}
	if disableAuth {
		return handler, nil
	}
	filter, err := rbacFilter()
	if err != nil {
		return nil, err
	}
	handler.authenticated, err = filter(ctrl.Log.WithName("metrics-auth"), http.HandlerFunc(handler.serveMetrics))
	if err != nil {
		return nil, err
	}
	return handler, nil
}

func promHandler() (*exporter, error) {
	cs, err := collector.Select(*collectorNames)
	if err != nil {
		return nil, err
	}
	if *scrapeTimeout <= 0 {
		return nil, fmt.Errorf("scrape-timeout must be positive")
	}
	return &exporter{run: (vtysh.Vtysh{}).Run, cs: cs, timeout: *scrapeTimeout,
		base: prometheus.DefaultGatherer, httpMetrics: prometheus.NewRegistry(), sem: make(chan struct{}, maxScrapes), responses: make(chan struct{}, maxResponses), logger: logging.GetLogger()}, nil
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
