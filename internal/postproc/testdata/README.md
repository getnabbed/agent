# Extraction fixtures

Made with 7-Zip 26.02 from `seq 1 40000 > Movie/movie.mkv` and
`seq 1 300 > movie.nfo`:

```sh
7z a -m0=lzma2 -mx=5 release.7z Movie movie.nfo
7z a -m0=copy -v100k split.7z Movie/movie.mkv
7z a -psecret -mhe=off secret.7z movie.nfo
```

RAR fixtures are built by the tests themselves (`rar5` in
`extract_test.go`) as stored RAR5, since RAR is not free to install.
