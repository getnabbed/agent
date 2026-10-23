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
	"github.com/getnabbed/agent/internal/credstore"
	"github.com/getnabbed/agent/internal/identity"
	"github.com/getnabbed/agent/internal/link"
	"github.com/getnabbed/agent/internal/lock"
	statestore "github.com/getnabbed/agent/internal/store"
	"github.com/getnabbed/agent/internal/tlsconf"
	"github.com/getnabbed/agent/internal/version"
)

const envAPIURL = "NABBED_API_URL"

type connFlags struct {
	apiURL string
	name   string
	caFile string
}

func (f *connFlags) register(fs *flag.FlagSet) {
	host, _ := os.Hostname()
	fs.StringVar(&f.apiURL, "api-url", os.Getenv(envAPIURL), "Nabbed API base URL (env "+envAPIURL+")")
	fs.StringVar(&f.name, "name", host, "name shown for this agent in the web app")
	fs.StringVar(&f.caFile, "ca-file", "", "extra PEM CA bundle for self-hosted control planes")
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

func httpClient(caFile string) (*http.Client, error) {
	cfg, err := tlsconf.New(tlsconf.Options{CAFile: caFile})
	if err != nil {
		return nil, err
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = cfg

	return &http.Client{Timeout: 30 * time.Second, Transport: tr}, nil
}

// pair runs the device flow and persists the result. It returns the
// credentials and the initial access token.
func pair(ctx context.Context, e *env, store *credstore.Store, f connFlags) (*credstore.Credentials, string, error) {
	if f.apiURL == "" {
		return nil, "", fmt.Errorf("--api-url (or %s) is required to pair", envAPIURL)
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
		AgentID: tr.AgentID, TenantID: tr.TenantID,
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
	f.register(fs)
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
	f.register(fs)
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

	applier := command.New(state, clk, e.log)
	applier.Prune(ctx)

	lc, err := link.New(link.Config{
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

	e.log.Info("agent starting", "agent_id", creds.AgentID, "gateway", creds.GatewayURL, "instance_id", lc.InstanceID())
	switch err := lc.Run(ctx); {
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

	store, err := credstore.Open(e.dataDir, e.log)
	if err != nil {
		e.log.Error("cannot open data dir", "err", err)
		return ExitError
	}

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
