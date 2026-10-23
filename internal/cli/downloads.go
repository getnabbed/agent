package cli

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"

	"github.com/getnabbed/agent/internal/auth"
	"github.com/getnabbed/agent/internal/config"
	"github.com/getnabbed/agent/internal/download"
	"github.com/getnabbed/agent/internal/engine"
	"github.com/getnabbed/agent/internal/engine/torrent"
	"github.com/getnabbed/agent/internal/engine/usenet"
	statestore "github.com/getnabbed/agent/internal/store"
)

type runFlags struct {
	downloadDir  string
	maxDownloads int
	torrentPort  int
	nntp         string
}

func (f *runFlags) register(fs *flag.FlagSet, cfg *config.Config) {
	fs.StringVar(&f.downloadDir, "download-dir", cfg.Downloads.Dir, "where downloads are written before import (env NABBED_DOWNLOAD_DIR)")
	fs.IntVar(&f.maxDownloads, "max-downloads", cfg.Downloads.MaxConcurrent, "downloads that may run at once (env NABBED_MAX_DOWNLOADS)")
	fs.IntVar(&f.torrentPort, "torrent-port", cfg.Torrent.Port, "BitTorrent listen port, 0 picks a free port (env NABBED_TORRENT_PORT)")
	// No env default here: flag defaults are printed by -h, and this one holds a
	// password. The env var is read in newDownloads instead.
	fs.StringVar(&f.nntp, "nntp", "",
		"TEMPORARY until sealed sources land: usenet provider as nntps://user:pass@host:563?connections=8.\n"+
			"Prefer NABBED_NNTP or NABBED_NNTP_FILE: flag values are visible in process lists")
}

// newDownloads builds the engines and the download manager. The returned
// func closes the engines.
func newDownloads(e *env, f runFlags, state *statestore.Store, api *url.URL, hc *http.Client, tokens *auth.TokenSource) (*download.Manager, func(), error) {
	var provider *usenet.Provider

	nntpURL := f.nntp
	if nntpURL == "" {
		nntpURL = string(e.cfg.Usenet.Provider)
	}

	if nntpURL != "" {
		p, err := usenet.ParseProviderURL(nntpURL)
		if err != nil {
			return nil, nil, fmt.Errorf("--nntp: %w", err)
		}

		provider = &p
		e.log.Warn("using a usenet provider from --nntp; this is temporary until sources are sealed to the agent", "provider", p.String())
	}

	nntp := usenet.New(
		func(context.Context, string) (usenet.Provider, error) {
			if provider == nil {
				return usenet.Provider{}, usenet.ErrNoProvider
			}

			return *provider, nil
		},
		func() bool { return provider != nil },
		e.log,
	)

	bt := torrent.New(torrent.Config{ListenPort: f.torrentPort, Log: e.log})

	m, err := download.New(download.Config{
		Store:         state,
		Engines:       []engine.Engine{nntp, bt},
		Fetcher:       &download.HTTPFetcher{API: api, Client: hc, Tokens: tokens},
		Dir:           f.downloadDir,
		MaxConcurrent: f.maxDownloads,
		Log:           e.log,
	})
	if err != nil {
		_ = bt.Close()
		return nil, nil, err
	}

	return m, func() { _ = bt.Close() }, nil
}
