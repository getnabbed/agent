Golden PAR2 files made by par2cmdline-turbo 1.5.0 (linux-amd64, sha256 of
the release zip 5a9f6438...aca464):

    seq 1 40000 > movie.mkv
    seq 1 300   > movie.nfo
    par2 c -q -s16384 -r20 -n2 release.par2 movie.mkv movie.nfo

Tests regenerate movie.mkv and movie.nfo in Go, so only the .par2 files are
committed.
