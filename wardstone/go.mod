module github.com/Wildstone-Systems/wildstone-homelab/wardstone

go 1.25.0

require (
	github.com/Wildstone-Systems/wildstone-homelab/keystone v0.0.0-00010101000000-000000000000
	github.com/Wildstone-Systems/wildstone-homelab/proto v0.0.0
	github.com/coreos/go-systemd/v22 v22.7.0
	golang.org/x/sys v0.47.0
	google.golang.org/grpc v1.74.2
	google.golang.org/protobuf v1.36.7
)

require (
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	golang.org/x/crypto v0.38.0
	golang.org/x/net v0.40.0 // indirect
	golang.org/x/text v0.25.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250528174236-200df99c418a // indirect
)

replace (
	github.com/Wildstone-Systems/wildstone-homelab/keystone => ../keystone
	github.com/Wildstone-Systems/wildstone-homelab/proto => ../proto
)
