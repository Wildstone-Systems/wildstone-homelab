module github.com/Wildstone-Systems/wildstone-homelab/wardstone

go 1.26.0

require (
	github.com/Wildstone-Systems/wildstone-homelab/keystone v0.0.0-00010101000000-000000000000
	github.com/Wildstone-Systems/wildstone-homelab/proto v0.0.0
	github.com/coreos/go-systemd/v22 v22.7.0
	golang.org/x/sys v0.49.0
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/godbus/dbus/v5 v5.2.4 // indirect
	golang.org/x/crypto v0.58.0
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261019180522-ce908d29299b // indirect
)

replace (
	github.com/Wildstone-Systems/wildstone-homelab/keystone => ../keystone
	github.com/Wildstone-Systems/wildstone-homelab/proto => ../proto
)
