module github.com/prezhdarov/vmware-exporter

go 1.26.0

// Minimum toolchain, not a preference. go1.26.0 through go1.26.5 carry seven
// standard-library advisories that govulncheck reports as reachable from this
// code (net/http, crypto/tls and encoding/* among them); 1.26.6 is the first
// release that fixes all of them.
//
// Without this line `actions/setup-go` with `go-version-file: go.mod` installs
// exactly the `go` directive version -- 1.26.0 -- and the vulnerability scan
// job fails on the standard library before it ever looks at a dependency.
toolchain go1.26.6

require (
	github.com/prometheus/client_golang v1.23.2
	github.com/prometheus/client_model v0.6.2
	github.com/prometheus/common v0.69.0
	github.com/prometheus/exporter-toolkit v0.16.0
	github.com/vmware/govmomi v0.56.0
	golang.org/x/sync v0.22.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/coreos/go-systemd/v22 v22.7.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jpillora/backoff v1.0.0 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/mdlayher/socket v0.6.1 // indirect
	github.com/mdlayher/vsock v1.3.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/mwitkow/go-conntrack v0.0.0-20190716064945-2f068394615f // indirect
	github.com/prometheus/procfs v0.20.1 // indirect
	go.yaml.in/yaml/v2 v2.4.4 // indirect
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
