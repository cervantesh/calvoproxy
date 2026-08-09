# CalvoProxy vault on headless Linux

CalvoProxy does not generate or persist a plaintext master key on Linux. The
service must receive exactly 32 raw random bytes from systemd credentials or an
explicitly mounted file. The encrypted provider vault belongs in the service
state directory; the master key does not.

## Recommended: systemd credentials

Create the service account and state directory through the unit rather than in
an interactive desktop session:

```ini
[Service]
User=calvoproxy
Group=calvoproxy
StateDirectory=calvoproxy
StateDirectoryMode=0700
LoadCredentialEncrypted=calvoproxy-vault-master-key:/etc/credstore.encrypted/calvoproxy-vault-master-key
Environment=PROXY_VAULT_FILE=/var/lib/calvoproxy/providers.vault
ExecStart=/usr/local/bin/calvoproxy
```

`LoadCredentialEncrypted=` makes systemd expose the decrypted credential at
runtime beneath `$CREDENTIALS_DIRECTORY`. CalvoProxy reads exactly this file:

```text
$CREDENTIALS_DIRECTORY/calvoproxy-vault-master-key
```

Provision 32 raw random bytes once, then encrypt them with `systemd-creds`:

```sh
sudo install -d -m 0700 /etc/credstore.encrypted
sudo systemd-creds encrypt --name=calvoproxy-vault-master-key - \
  /etc/credstore.encrypted/calvoproxy-vault-master-key < <(head -c 32 /dev/urandom)
sudo systemctl daemon-reload
sudo systemctl restart calvoproxy
```

The process identity must remain stable. Changing `User=` can make a
user-scoped deployment inaccessible, and changing the machine or TPM may
require reprovisioning an encrypted system credential. Back up the 32-byte key
through an appropriately protected recovery process; a backup of
`providers.vault` alone is intentionally unusable.

The vault path should be inside the state directory, for example:

```text
/var/lib/calvoproxy/providers.vault
```

Do not put the master key in `Environment=`, an environment file, the command
line, the container image, Git, or the same state directory as the vault.

## Docker or non-systemd services

Mount a file containing exactly 32 raw bytes and point CalvoProxy at it:

```yaml
services:
  calvoproxy:
    volumes:
      - ./data:/var/lib/calvoproxy
      - type: bind
        source: /secure/calvoproxy-vault-master-key
        target: /run/secrets/calvoproxy-vault-master-key
        read_only: true
    environment:
      PROXY_VAULT_MASTER_KEY_FILE: /run/secrets/calvoproxy-vault-master-key
```

The mounted path must be a regular file, not a symbolic link. It must not grant
group or world permissions (`0400` or `0600` are accepted). CalvoProxy rejects
missing, short, encoded, newline-terminated, overly permissive, and symlinked
keys. In particular, store raw bytes rather than 64 hexadecimal characters or
base64 text.

When `$CREDENTIALS_DIRECTORY` is present, the systemd credential is
authoritative and the mounted-file fallback is not attempted. This prevents a
misconfigured service from silently opening a vault with an unintended key.

## Platform note

Windows builds create a 32-byte master key and store only its DPAPI-protected
blob next to the encrypted vault. It is protected for the current Windows user
and DPAPI UI is disabled. The plaintext key is never written to disk.

Native CGO-enabled macOS builds create and load only the 32-byte master key via
Security.framework Keychain APIs. They do not shell out to the `security`
command and do not use a plaintext fallback. A deliberately CGO-disabled macOS
build cannot link Security.framework and therefore keeps the vault locked.
