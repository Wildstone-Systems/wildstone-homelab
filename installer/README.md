# installer/

Bootstrap installer and preflight checks. Per decision D2 (you-first), the
bootstrap script is fetched from GitHub releases; no hosted install endpoint
until a second user exists. Installs Docker CE if absent, installs
wardstoned (systemd), and pulls up Keystone; onboarding continues in the
Console, and the Restore path branches from the same flow.
