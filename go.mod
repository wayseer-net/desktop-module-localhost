module wayseer.dev/modules/localhost

go 1.27

require (
	github.com/ebitengine/purego v0.11.0
	github.com/godbus/dbus/v5 v5.2.2
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/net v0.57.0
	golang.org/x/sys v0.48.0
	wayseer.dev/sdk v0.1.0
)

// Until wayseer.dev serves the SDK's page, fetch it from GitHub (GOPRIVATE and git credentials).
replace wayseer.dev/sdk => github.com/wayseer-net/desktop-sdk v0.1.0
