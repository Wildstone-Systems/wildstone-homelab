module github.com/Wildstone-Systems/wildstone-homelab/cli

go 1.25.0

require github.com/Wildstone-Systems/wildstone-homelab/keystone v0.0.0

require gopkg.in/yaml.v3 v3.0.1 // indirect

replace github.com/Wildstone-Systems/wildstone-homelab/keystone => ../keystone

replace github.com/Wildstone-Systems/wildstone-homelab/proto => ../proto
