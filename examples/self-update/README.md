# Updating the server from the Management Center

The Management Center's **System > Check for updates** can install a newer release of the server
on a systemd host. The server itself never replaces its binary: it only checks GitHub for a newer
release and writes an update request. A small root helper installed on the host does the rest.

1. `GET /v8/management/server/update` checks `remote-management.server-update-repository` for its
   latest release and reports whether it is newer than the running version and whether the release
   archive for this platform has been uploaded yet.
2. `POST /v8/management/server/update` writes `request.json` (the tag only) into
   `remote-management.server-update-dir`.
3. `cliproxy-self-update.path` sees the file and starts `cliproxy-self-update.service`, which runs
   `cliproxy-self-update.sh` as root. The helper downloads the release archive from the repository
   in **its own** config, checks it against the SHA-256 digest GitHub publishes for the asset,
   checks that the binary reports the requested version, swaps it in, restarts the service and
   rolls back to the previous binary if the server doesn't come up healthy.
4. The helper reports progress in `status.json` in the same directory, which the panel shows.

Releases come from the repository's release workflow (`.github/workflows/release.yaml`): pushing a
tag such as `v8.1.5` builds the archives, including `CLIProxyAPI_<version>_linux_<arch>_no-plugin.tar.gz`.

## Install

```bash
sudo install -m 755 cliproxy-self-update.sh /usr/local/sbin/cliproxy-self-update
sudo install -m 644 cliproxy-self-update.env.example /etc/cliproxy-self-update.env
sudoedit /etc/cliproxy-self-update.env          # REPO and UPDATE_DIR
sudo install -d -o cliproxy -g cliproxy -m 750 /home/cliproxy/cliproxy/update
sudo install -m 644 cliproxy-self-update.service cliproxy-self-update.path /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cliproxy-self-update.path
```

The helper needs `curl`, `jq`, `tar` and `runuser`. Then set the same repository and directory in
`config.yaml`:

```yaml
remote-management:
  server-update-repository: "https://github.com/router-for-me/CLIProxyAPI"
  server-update-dir: "/home/cliproxy/cliproxy/update"
```

Adjust `PathExists=` in the path unit if you use another directory. Follow a run with
`journalctl -u cliproxy-self-update`. To test the download and verification without installing,
run the helper with `DRY_RUN=1` against a request file in a scratch directory (set
`CLIPROXY_SELF_UPDATE_CONF` to a copy of the config whose `UPDATE_DIR` points there).

## Trust

Anyone with the management key can request an update, so the request carries nothing but a tag.
The repository that may be installed from is fixed in the root-owned
`/etc/cliproxy-self-update.env`, so changing `config.yaml` through the Management API cannot
redirect the helper to another repository. The helper only reads and writes files in the shared
directory as the service user.
