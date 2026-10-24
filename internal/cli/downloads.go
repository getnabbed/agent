package cli

import (
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
	"github.com/getnabbed/agent/internal/postproc"
	statestore "github.com/getnabbed/agent/internal/store"
)

type runFlags struct {
	downloadDir  string
	maxDownloads int
	torrentPort  int
	allowLAN     bool
	nntp         string
}

func (f *runFlags) register(fs *flag.FlagSet, cfg *config.Config) {
	fs.StringVar(&f.downloadDir, "download-dir", cfg.Downloads.Dir, "where downloads are written before import (env NABBED_DOWNLOAD_DIR)")
	fs.IntVar(&f.maxDownloads, "max-downloads", cfg.Downloads.MaxConcurrent, "downloads that may run at once (env NABBED_MAX_DOWNLOADS)")
	fs.IntVar(&f.torrentPort, "torrent-port", cfg.Torrent.Port, "BitTorrent listen port, 0 picks a free port (env NABBED_TORRENT_PORT)")
	fs.BoolVar(&f.allowLAN, "torrent-allow-lan", cfg.Torrent.AllowLAN, "let torrents reach peers, trackers and web seeds on the local network (env NABBED_TORRENT_ALLOW_LAN)")
	// No env default here: flag defaults are printed by -h, and this one holds a
	// password. The env var is read in newDownloads instead.
	fs.StringVar(&f.nntp, "nntp", "",
		"TEMPORARY until sealed sources land: usenet providers as nntps://user:pass@host:563?connections=8,\n"+
			"space-separated in priority order; add &backup=true for fill-only block accounts.\n"+
			"Prefer NABBED_NNTP or NABBED_NNTP_FILE: flag values are visible in process lists")
}

// newDownloads builds the engines and the download manager. The returned
// func closes the engines.
func newDownloads(e *env, f runFlags, state *statestore.Store, api *url.URL, hc *http.Client, tokens *auth.TokenSource, imp download.Importer) (*download.Manager, *usenet.Engine, func(), error) {
	nntpURLs := f.nntp
	if nntpURLs == "" {
		nntpURLs = string(e.cfg.Usenet.Provider)
	}

	providers, err := usenet.ParseProviderList(nntpURLs)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("usenet provider: %w", err)
	}

	nntp := usenet.New(e.log)
	for i, p := range providers {
		// The colon keeps these ids apart from control-plane source ids.
		nntp.SetProvider(fmt.Sprintf("local:%d", i+1), p)
		e.log.Warn("using a usenet provider from configuration; this is temporary until sources are sealed to the agent", "provider", p.String())
	}

	bt := torrent.New(torrent.Config{ListenPort: f.torrentPort, AllowLAN: f.allowLAN, Log: e.log})
	if f.allowLAN {
		e.log.Warn("torrents may contact devices on the local network (torrent.allow_lan is on)")
	}

	m, err := download.New(download.Config{
		Store:         state,
		Engines:       []engine.Engine{nntp, bt},
		Fetcher:       &download.HTTPFetcher{API: api, Client: hc, Tokens: tokens},
		Dir:           f.downloadDir,
		MaxConcurrent: f.maxDownloads,
		// The usenet engine fetches the PAR2 volumes it deferred when repair
		// needs them.
		Assembler: &postproc.Assembler{Log: e.log, Fetcher: nntp},
		Importer:  imp,
		Log:       e.log,
	})
	if err != nil {
		_ = bt.Close()
		_ = nntp.Close()

		return nil, nil, nil, err
	}

	return m, nntp, func() {
		_ = bt.Close()
		_ = nntp.Close()
	}, nil
}
