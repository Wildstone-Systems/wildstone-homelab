package e2e

import (
	"bytes"
	"fmt"
	"os"

	"github.com/kdomanski/iso9660"
)

// UserData renders a minimal cloud-init NoCloud user-data document that
// creates a login user with the given SSH public key.
func UserData(user, authorizedKey string) string {
	return fmt.Sprintf(`#cloud-config
users:
  - name: %s
    groups: [sudo]
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    lock_passwd: true
    ssh_authorized_keys:
      - %s
ssh_pwauth: false
`, user, authorizedKey)
}

// WriteSeedISO writes a NoCloud seed ISO (volume label "cidata") containing
// user-data and meta-data to path.
func WriteSeedISO(path, instanceID, userData string) error {
	w, err := iso9660.NewWriter()
	if err != nil {
		return err
	}
	defer w.Cleanup()

	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", instanceID, instanceID)
	if err := w.AddFile(bytes.NewReader([]byte(userData)), "user-data"); err != nil {
		return err
	}
	if err := w.AddFile(bytes.NewReader([]byte(metaData)), "meta-data"); err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return w.WriteTo(f, "cidata")
}
