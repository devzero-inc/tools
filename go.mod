module golang.org/x/tools

go 1.23.0 // => default GODEBUG has gotypesalias=0

toolchain go1.23.7

require (
	github.com/google/go-cmp v0.6.0
	github.com/yuin/goldmark v1.4.13
	golang.org/x/mod v0.22.0
	golang.org/x/net v0.36.0
	golang.org/x/sync v0.10.0
	golang.org/x/telemetry v0.0.0-20240521205824-bda55230c457
)

require golang.org/x/sys v0.30.0 // indirect
