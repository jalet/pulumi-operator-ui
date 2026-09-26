// Command pulumi-operator-ui serves a read-only UI for Pulumi Kubernetes Operator stacks.
package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // the distroless base image may ship no zone files

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/go-logr/zerologr"
	"github.com/rs/zerolog"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/jalet/pulumi-operator-ui/internal/auth"
	"github.com/jalet/pulumi-operator-ui/internal/config"
	"github.com/jalet/pulumi-operator-ui/internal/events"
	"github.com/jalet/pulumi-operator-ui/internal/logs"
	"github.com/jalet/pulumi-operator-ui/internal/s3hist"
	"github.com/jalet/pulumi-operator-ui/internal/store"
	"github.com/jalet/pulumi-operator-ui/internal/watch"
	"github.com/jalet/pulumi-operator-ui/internal/web"
)

const (
	pruneInterval   = time.Hour
	shutdownTimeout = 10 * time.Second
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	loadAWS := func(ctx context.Context) (aws.Config, error) {
		return awsconfig.LoadDefaultConfig(ctx)
	}
	if err := run(ctx, os.Args[1:], os.Getenv, ctrl.GetConfig, loadAWS); err != nil {
		fmt.Fprintln(os.Stderr, err)
		stop()
		os.Exit(1) //nolint:gocritic // stop() was called explicitly above
	}
}

type secrets struct {
	clientSecret string
	codec        *auth.Codec
	oidcRoots    *x509.CertPool
}

func run(ctx context.Context, args []string, getenv func(string) string,
	restConfig func() (*rest.Config, error),
	loadAWS func(ctx context.Context) (aws.Config, error)) error {
	cfg, err := config.Parse(args, getenv)
	if err != nil {
		return err
	}
	logger := zerolog.New(os.Stdout).With().Timestamp().Str("version", version).Logger()
	ctrl.SetLogger(zerologr.New(&logger))
	sec, err := loadSecrets(cfg)
	if err != nil {
		return err
	}
	broker := events.NewBroker()
	st, err := store.Open(ctx, store.Options{URL: cfg.DatabaseURL, CAFile: cfg.DatabaseCAFile,
		Pub: broker, Log: logger})
	if err != nil {
		return err
	}
	defer st.Close()
	rc, err := restConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	mgr, err := watch.NewManager(rc, watch.Options{Namespaces: cfg.Namespaces,
		MetricsAddr: cfg.MetricsAddr, Writer: st, Now: time.Now})
	if err != nil {
		return err
	}
	authn, err := auth.New(ctx, auth.Config{
		Issuer: cfg.OIDC.Issuer, ClientID: cfg.OIDC.ClientID, ClientSecret: sec.clientSecret,
		RedirectURL: cfg.OIDC.RedirectURL, CAPool: sec.oidcRoots, Claim: cfg.AuthClaim,
		Allowed: cfg.AuthAllowed, SessionAgeMax: cfg.SessionAgeMax,
	}, sec.codec, st, logger, time.Now)
	if err != nil {
		return err
	}
	var s3Interval time.Duration
	if cfg.S3HistoryEnabled {
		s3Interval = cfg.S3HistoryInterval
	}
	loc, err := time.LoadLocation(cfg.DisplayTimezone) // validated by config.Parse
	if err != nil {
		return fmt.Errorf("display timezone: %w", err)
	}
	handler := web.New(web.Deps{Store: st, Broker: broker, RequireAuth: authn.Require,
		AuthRoutes: authn.Routes, Log: logger, Now: time.Now, S3Interval: s3Interval,
		Location: loc, ThemeCSS: cfg.Theme.CSS()})
	srv := newHTTPServer(cfg.HTTPAddr, handler)
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return serveHTTP(ctx, srv)
	})); err != nil {
		return fmt.Errorf("add http server: %w", err)
	}
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return st.RunPruner(ctx, pruneInterval, cfg.RetentionRuns, cfg.RetentionAuth)
	})); err != nil {
		return fmt.Errorf("add pruner: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return fmt.Errorf("kubernetes clientset: %w", err)
	}
	capturer := logs.New(logs.Options{Store: st, Source: logs.NewKubeSource(cs), Now: time.Now,
		Log: logger})
	if err := mgr.Add(manager.RunnableFunc(capturer.Run)); err != nil {
		return fmt.Errorf("add log capture: %w", err)
	}
	if cfg.S3HistoryEnabled {
		if err := addS3History(ctx, mgr, cfg, st, logger, loadAWS); err != nil {
			return err
		}
	}
	logger.Info().Str("http", cfg.HTTPAddr).Str("metrics", cfg.MetricsAddr).
		Strs("namespaces", cfg.Namespaces).Msg("starting")
	return mgr.Start(ctx)
}

// addS3History loads AWS config and starts the S3 history poller. It is only called when S3
// history is enabled, so the app never touches AWS configuration otherwise.
func addS3History(ctx context.Context, mgr manager.Manager, cfg config.Config, st *store.Store,
	logger zerolog.Logger, loadAWS func(ctx context.Context) (aws.Config, error)) error {
	awsCfg, err := loadAWS(ctx)
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}
	poller := s3hist.New(s3hist.Options{Store: st, Client: newS3Clients(awsCfg),
		Interval: cfg.S3HistoryInterval, Retention: cfg.RetentionRuns, Now: time.Now,
		Log: logger})
	if err := mgr.Add(manager.RunnableFunc(poller.Run)); err != nil {
		return fmt.Errorf("add s3 history poller: %w", err)
	}
	return nil
}

// newS3Clients returns one S3 client per region, created on first use. Region "" keeps the
// SDK's default region.
func newS3Clients(cfg aws.Config) func(region string) (s3hist.S3API, error) {
	var mu sync.Mutex
	clients := map[string]*s3.Client{}
	return func(region string) (s3hist.S3API, error) {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := clients[region]; ok {
			return c, nil
		}
		c := s3.NewFromConfig(cfg, func(o *s3.Options) {
			if region != "" {
				o.Region = region
			}
		})
		clients[region] = c
		return c, nil
	}
}

func loadSecrets(cfg config.Config) (secrets, error) {
	b, err := os.ReadFile(cfg.OIDC.ClientSecretFile) //nolint:gosec // operator-configured path
	if err != nil {
		return secrets{}, fmt.Errorf("read client secret: %w", err)
	}
	clientSecret := string(bytes.TrimSpace(b))
	if clientSecret == "" {
		return secrets{}, errors.New("client secret file is empty")
	}
	current, err := auth.LoadKey(cfg.SessionKeyFile)
	if err != nil {
		return secrets{}, err
	}
	var previous []byte
	if cfg.SessionPrevKeyFile != "" {
		if previous, err = auth.LoadKey(cfg.SessionPrevKeyFile); err != nil {
			return secrets{}, err
		}
	}
	codec, err := auth.NewCodec(current, previous)
	if err != nil {
		return secrets{}, err
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return secrets{}, fmt.Errorf("system cert pool: %w", err)
	}
	if cfg.OIDC.CAFile != "" {
		pem, err := os.ReadFile(cfg.OIDC.CAFile) //nolint:gosec // operator-configured path
		if err != nil {
			return secrets{}, fmt.Errorf("read OIDC CA: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return secrets{}, errors.New("read OIDC CA: no certificates found")
		}
	}
	return secrets{clientSecret: clientSecret, codec: codec, oidcRoots: roots}, nil
}

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second, // the SSE handler clears its own deadline
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

// serveHTTP runs srv until ctx ends. Request contexts derive from ctx, so open SSE
// streams end on shutdown instead of holding Shutdown until its timeout.
func serveHTTP(ctx context.Context, srv *http.Server) error {
	srv.BaseContext = func(net.Listener) context.Context { return ctx }
	errCh := make(chan error, 1)                  // single-result handoff from ListenAndServe
	go func() { errCh <- srv.ListenAndServe() }() // ends when Shutdown is called below
	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		return nil
	}
}
