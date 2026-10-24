Golden PAR2 files made by par2cmdline-turbo 1.5.0 (linux-amd64, sha256 of
the release zip 5a9f6438...aca464). Tests regenerate the data files in Go,
so only the .par2 files are committed.

`release.*`: 16 KiB slices, 15 input slices, 3 recovery slices.

    seq 1 40000 > movie.mkv
    seq 1 300   > movie.nfo
    par2 c -q -s16384 -r20 -n2 release.par2 movie.mkv movie.nfo

`s4096.*`: the same data in 4 KiB slices, 57 input slices, 12 recovery
slices in volumes of 1, 2, 4 and 5.

    par2 c -q -s4096 -c12 s4096.par2 movie.mkv movie.nfo

`odd.*`: random data in 6000-byte slices (not a power of two). bin.dat is
100001 bytes, so its last slice is 4001 bytes and ends mid-word. Recovery
exponents start at 1000.

    par2 c -q -s6000 -c8 -f1000 -n2 odd.par2 bin.dat small.txt

bin.dat and small.txt are the first 100001 and 777 bytes of Go's
`math/rand/v2` ChaCha8 stream seeded with a 32-byte seed whose first byte is
1 and 2 respectively, all other bytes zero (`random` in repair_test.go).

After each repair test case was first written, its repaired output was
checked with `par2 v`, which reported "All files are correct". Tests do not
need the binary.
