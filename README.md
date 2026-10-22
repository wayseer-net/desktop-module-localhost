# Wayseer module: localhost

The `localhost` module of Wayseer Desktop: the machine Wayseer runs on. It lists the host, its
CPUs, memory, disks, filesystems, network interfaces and processes, with series for their use.
Where the machine runs systemd it adds the system units and their dependencies, and journal
entries as events; on Windows it adds the services, from the service control manager. With
`connections: true` on Linux, it adds the machine's network connections: each remote address,
which process talks to it and at how many bytes per second, and the listening sockets; with
`geoip` databases in MMDB format (DB-IP Lite or MaxMind GeoLite2, which the user downloads), it
places public remotes on the map and names their networks. On Linux it reads `/proc`, `/sys` and sock_diag; on macOS, sysctls and Mach calls; on
Windows, Win32 calls through `golang.org/x/sys/windows`.

```
go get wayseer.dev/modules/localhost
```

Wayseer links it in, so users do not install it. Its options and what it shows are in Wayseer's
user guide, under "This machine".

`testdata` holds a made-up Linux tree (`procfs`), systemd's answers, journal entries and a
made-up Windows machine's answers (`windows`), which the tests read instead of the machine, on
any OS. The sock_diag parser is tested on messages built in the tests, and
`TestLiveSockets` reads this machine's own sockets on Linux. The geoip tests write small databases of
documentation addresses with `mmdbwriter`. `scripts/recordunits.sh` re-records
`testdata/systemd/stops.json` on a machine with systemd.

## Working on it

```
make check   # what CI runs: tests with the conformance suite, vet and lint for every platform, a key scan
make help    # every target
```

It imports only the SDK (`wayseer.dev/sdk`), the standard library and its own
dependencies; `TestImportsOnlyTheSDK` keeps it that way. golangci-lint is pinned in
`tools/go.mod`, and gitleaks runs at a pinned version through `go run`.

To change it alongside the SDK or the app, use a Go workspace: `go.work` here with
`use . ../../sdk`, or the app's `make workspace`, which writes one for the whole of Wayseer
Desktop. `go.work` is ignored by git.

## Licence

MIT; see `LICENSE`.
