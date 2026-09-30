module github.com/chickenrat17/wildstone-homelab/keystone

go 1.25.0

require (
	github.com/chickenrat17/wildstone-homelab/proto v0.0.0
	github.com/yuin/goldmark v1.7.13
	golang.org/x/net v0.40.0
	google.golang.org/grpc v1.74.2
	google.golang.org/protobuf v1.36.7
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.25.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250528174236-200df99c418a // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.56.0 // indirect
)

replace github.com/chickenrat17/wildstone-homelab/proto => ../proto
