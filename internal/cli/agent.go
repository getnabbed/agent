package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	"google.golang.org/grpc/credentials"

	"github.com/getnabbed/agent/internal/auth"
	"github.com/getnabbed/agent/internal/clock"
	"github.com/getnabbed/agent/internal/command"
	"github.com/getnabbed/agent/internal/config"
	"github.com/getnabbed/agent/internal/credstore"
	"github.com/getnabbed/agent/internal/identity"
	"github.com/getnabbed/agent/internal/library"
	"github.com/getnabbed/agent/internal/link"
	"github.com/getnabbed/agent/internal/lock"
	"github.com/getnabbed/agent/internal/source"
	statestore "github.com/getnabbed/agent/internal/store"
	"github.com/getnabbed/agent/internal/tlsconf"
	"github.com/getnabbed/agent/internal/version"
)

type connFlags struct {
	apiURL string
	name   string
	caFile string
}

// register binds the flags with defaults from the resolved config, so a
// flag overrides the file and environment only when given.
func (f *connFlags) register(fs *flag.FlagSet, cfg *config.Config) {
	fs.StringVar(&f.apiURL, "api-url", cfg.APIURL, "Nabbed API base URL (env NABBED_API_URL)")
	fs.StringVar(&f.name, "name", cfg.Name, "name shown for this agent in the web app (env NABBED_NAME)")
	fs.StringVar(&f.caFile, "ca-file", cfg.TLS.CAFile, "extra PEM CA bundle for self-hosted control planes (env NABBED_CA_FILE)")
}

func userAgent() string {
	return fmt.Sprintf("nabbed-agent/%s (%s/%s)", version.Version, runtime.GOOS, runtime.GOARCH)
}

func osString() string { return runtime.GOOS + "/" + runtime.GOARCH }

// openLocked opens the store and takes the data-dir lock.
func openLocked(e *env) (*credstore.Store, *lock.Lock, error) {
	store, err := credstore.Open(e.dataDir, e.log)
	if err != nil {
		return nil, nil, err
	}

	l, err := lock.Acquire(store.Dir())
	if errors.Is(err, lock.ErrLocked) {
		return nil, nil, fmt.Errorf("%w (%s); stop the running agent first", err, store.Dir())
	}
	if err != nil {
		return nil, nil, err
	}

	return store, l, nil
}

// httpClient is the client for cloud-api. It never follows redirects: a
// redirect would re-send refresh tokens, proofs and device codes to
// whatever host it names, possibly over plain http. Payload downloads set
// their own, stricter policy (see download.HTTPFetcher).
func httpClient(caFile string) (*http.Client, error) {
	cfg, err := tlsconf.New(tlsconf.Options{CAFile: caFile})
	if err != nil {
		return nil, err
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = cfg

	return &http.Client{
		Timeout:       30 * time.Second,
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// pair runs the device flow and persists the result. It returns the
// credentials and the initial access token.
func pair(ctx context.Context, e *env, store *credstore.Store, f connFlags) (*credstore.Credentials, string, error) {
	if f.apiURL == "" {
		return nil, "", errors.New("--api-url (or NABBED_API_URL, or api_url in the config file) is required to pair")
	}

	api, err := auth.ParseAPIURL(f.apiURL)
	if err != nil {
		return nil, "", err
	}

	hc, err := httpClient(f.caFile)
	if err != nil {
		return nil, "", err
	}

	id, err := identity.Generate()
	if err != nil {
		return nil, "", err
	}

	client := auth.NewClient(api, hc, userAgent())

	tr, err := auth.Pair(ctx, client, id, auth.PairOptions{
		ClientName: f.name,
		OS:         osString(),
		Version:    version.Version,
		Log:        e.log,
		Prompt: func(ar *auth.AuthorizeResponse) {
			uri := ar.VerificationURI
			if ar.VerificationURIComplete != "" {
				uri = ar.VerificationURIComplete
			}

			// The log line carries the code for headless installs (docker logs).
			e.log.Info("pairing required: approve this agent in the web app",
				"verification_uri", uri, "user_code", ar.UserCode, "fingerprint", id.Thumbprint()[:16])
			fmt.Fprintf(e.stdout, "\nTo pair this agent, open:\n\n    %s\n\nand enter the code:\n\n    %s\n\nAgent key fingerprint: %s\n\n",
				uri, ar.UserCode, id.Thumbprint()[:16])
		},
	})
	if err != nil {
		return nil, "", err
	}

	gw, err := auth.ValidateGatewayURL(api, tr.GatewayURL)
	if err != nil {
		return nil, "", fmt.Errorf("refusing gateway from pairing response: %w", err)
	}

	creds := &credstore.Credentials{
		AgentID: string(tr.AgentID), TenantID: string(tr.TenantID),
		APIURL: api.String(), GatewayURL: gw.String(),
		SignSeed: id.SignSeed(), SealKey: id.SealKey(),
		RefreshToken: tr.RefreshToken,
		PairedAt:     time.Now().UTC(),
	}

	if err := store.Save(creds); err != nil {
		return nil, "", fmt.Errorf("save credentials: %w", err)
	}

	e.log.Info("agent paired", "agent_id", creds.AgentID, "tenant_id", creds.TenantID, "protector", store.Protector().Name())

	return creds, tr.AccessToken, nil
}

func runPair(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	var f connFlags
	f.register(fs, e.cfg)
	force := fs.Bool("force", false, "replace existing credentials with a new identity")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	store, l, err := openLocked(e)
	if err != nil {
		e.log.Error("cannot open data dir", "err", err)
		return ExitError
	}

	defer func() { _ = l.Release() }()

	if existing, err := store.Load(); err == nil && !*force {
		e.log.Error("agent is already paired; use --force to replace its identity", "agent_id", existing.AgentID)
		return ExitError
	}

	if _, _, err := pair(ctx, e, store, f); err != nil {
		if ctx.Err() != nil {
			return ExitError
		}

		e.log.Error("pairing failed", "err", err)
		return ExitError
	}

	return ExitOK
}

func runRun(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	var f connFlags
	f.register(fs, e.cfg)

	var rf runFlags
	rf.register(fs, e.cfg)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	store, l, err := openLocked(e)
	if err != nil {
		e.log.Error("cannot open data dir", "err", err)
		return ExitError
	}

	defer func() { _ = l.Release() }()

	creds, err := store.Load()
	var seed string
	if errors.Is(err, credstore.ErrNotPaired) {
		e.log.Info("no credentials found; starting pairing", "data_dir", store.Dir())
		creds, seed, err = pair(ctx, e, store, f)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ExitOK
		}

		e.log.Error("cannot load credentials", "err", err)
		return ExitError
	}

	id, err := identity.FromSecrets(creds.SignSeed, creds.SealKey)
	if err != nil {
		e.log.Error("credentials are corrupt", "err", err)
		return ExitError
	}

	api, err := auth.ParseAPIURL(creds.APIURL)
	if err != nil {
		e.log.Error("stored api url is invalid", "err", err)
		return ExitError
	}

	hc, err := httpClient(f.caFile)
	if err != nil {
		e.log.Error("tls setup failed", "err", err)
		return ExitError
	}

	clk := clock.New()
	tokens := auth.NewTokenSource(auth.NewClient(api, hc, userAgent()), store, id, creds, clk, e.log)
	if seed != "" {
		if err := tokens.Seed(seed); err != nil {
			e.log.Warn("ignoring initial access token", "err", err)
		}
	}

	tlsCfg, err := tlsconf.New(tlsconf.Options{CAFile: f.caFile})
	if err != nil {
		e.log.Error("tls setup failed", "err", err)
		return ExitError
	}

	state, err := statestore.Open(ctx, store.Dir(), creds.AgentID, e.log)
	if err != nil {
		e.log.Error("cannot open state database", "err", err)
		return ExitError
	}

	defer func() { _ = state.Close() }()

	lib, err := newLibrary(e, state)
	if err != nil {
		e.log.Error("library setup failed", "err", err)
		return ExitError
	}

	downloads, nntp, closeEngines, err := newDownloads(e, rf, state, api, hc, tokens, &library.Importer{Library: lib, Log: e.log})
	if err != nil {
		e.log.Error("download setup failed", "err", err)
		return ExitError
	}

	defer closeEngines()

	sources := source.New(source.Config{Store: state, Key: id.SealPrivate(), Engine: nntp, Log: e.log})
	if err := sources.Load(ctx); err != nil {
		e.log.Error("cannot load download sources", "err", err)
		return ExitError
	}

	applier := command.New(command.Config{Store: state, Clock: clk, Downloads: downloads, Sources: sources, Library: lib, Log: e.log})
	applier.Prune(ctx)

	lc, err := link.New(link.Config{
		Capabilities:         capabilities(downloads, lib),
		Sources:              sources.Inventory,
		GatewayURL:           creds.GatewayURL,
		TransportCredentials: credentials.NewTLS(tlsCfg),
		Tokens:               tokens,
		Signer:               id,
		State:                state,
		Handler:              applier,
		Outbox:               state,
		Clock:                clk,
		Logger:               e.log,
		Version:              version.Version,
		OS:                   osString(),
	})
	if err != nil {
		e.log.Error("link setup failed", "err", err)
		return ExitError
	}

	defer func() { _ = lc.Close() }()

	downloads.SetTelemetry(lc)

	e.log.Info("agent starting", "agent_id", creds.AgentID, "gateway", creds.GatewayURL,
		"instance_id", lc.InstanceID(), "capabilities", downloads.Capabilities())

	// Downloads keep running while the link reconnects; both stop when the
	// link gives up for good or the process is asked to stop.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	dlDone := make(chan struct{})
	go func() {
		defer close(dlDone)
		_ = downloads.Run(runCtx)
	}()

	// Checks each source as it is applied and queues SourceHealth events.
	srcDone := make(chan struct{})
	go func() {
		defer close(srcDone)
		_ = sources.Run(runCtx)
	}()

	// Housekeeping, so a long-running agent's database stays bounded.
	go maintain(runCtx, time.Hour, func(ctx context.Context) {
		applier.Prune(ctx)
		downloads.GC(ctx)
	})

	err = lc.Run(runCtx)
	stop()
	<-dlDone
	<-srcDone

	switch {
	case err == nil:
		e.log.Info("agent stopped")
		return ExitOK
	case errors.Is(err, link.ErrRevoked), errors.Is(err, auth.ErrReauthRequired):
		e.log.Error("agent can no longer authenticate; run 'agent pair --force' to pair it again", "err", err)
		return ExitReauth
	default:
		e.log.Error("agent stopped", "err", err)
		return ExitError
	}
}

func runStatus(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	// Read-only: inspecting a data dir must never create or re-permission it.
	store := credstore.OpenExisting(e.dataDir, e.log)

	creds, err := store.Load()
	if errors.Is(err, credstore.ErrNotPaired) {
		fmt.Fprintf(e.stdout, "not paired\ndata dir:    %s\n", store.Dir())
		return ExitOK
	}
	if err != nil {
		e.log.Error("cannot load credentials", "err", err)
		return ExitError
	}

	id, err := identity.FromSecrets(creds.SignSeed, creds.SealKey)
	if err != nil {
		e.log.Error("credentials are corrupt", "err", err)
		return ExitError
	}

	refreshed := "never"
	if !creds.RefreshedAt.IsZero() {
		refreshed = creds.RefreshedAt.Format(time.RFC3339)
	}

	fmt.Fprintf(e.stdout, "agent id:    %s\ntenant id:   %s\napi:         %s\ngateway:     %s\nfingerprint: %s\npaired:      %s\nrefreshed:   %s\ndata dir:    %s\n",
		creds.AgentID, creds.TenantID, creds.APIURL, creds.GatewayURL, id.Thumbprint(),
		creds.PairedAt.Format(time.RFC3339), refreshed, store.Dir())
	if creds.PendingRefresh != nil {
		fmt.Fprintf(e.stdout, "note:        a token refresh was interrupted at %s and will be retried\n", creds.PendingRefresh.StartedAt.Format(time.RFC3339))
	}

	sum, err := statestore.Inspect(ctx, store.Dir())
	switch {
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintln(e.stdout, "state:       none yet (created on first run)")
	case err != nil:
		e.log.Warn("cannot read state database", "err", err)
	default:
		last := sum.LastCommandID
		if last == "" {
			last = "none"
		}

		fmt.Fprintf(e.stdout, "state:       schema v%d, %d commands applied, last %s, %d events pending\n",
			sum.SchemaVersion, sum.AppliedCount, last, sum.PendingEvents)

		if sum.AgentID != "" && sum.AgentID != creds.AgentID {
			fmt.Fprintf(e.stdout, "note:        state belongs to previous agent %s and will be set aside on next run\n", sum.AgentID)
		}
	}

	return ExitOK
}

// maintain runs fn every interval until ctx ends.
func maintain(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}
