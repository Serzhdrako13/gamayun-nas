// Package version — версия сборки gamayund и gamayun-helper.
package version

// Version подставляется при сборке:
//
//	go build -ldflags "-X gamayun-nas/internal/version.Version=0.1.0" ./cmd/...
var Version = "dev"
