# Restore a Hermes Box v2 backup

Hermes Box v2 recovery bundles are self-contained, age-encrypted archives of
durable guest state and the verified install inputs needed to reconstruct the
applied root. They are not v1 portable packages and do not contain a smolvm
image, root filesystem, Lima binary, or reusable SSH key.

You need:

- an Apple Silicon Mac with Go 1.24 or newer;
- Lima 2.1.3, or the exact qualified version required by the applied lock;
- an absent destination box with a reviewed `hermes-box.yaml`; and
- the exported age identity matching the backup envelope.

Restore:

```bash
./bin/hermes-box \
  --config /path/to/destination/hermes-box.yaml \
  restore /path/to/backup.tar.zst.age \
  --identity /secure/path/source-age-key.txt
```

Restore refuses an existing VM or data disk. It verifies and decrypts the
archive, validates safe paths, creates a fresh persistent disk and Ubuntu root,
restores `/data`, applies the archived root lock, and checks health.
Archive, envelope, identity, manifest, path, and artifact-closure verification
all finish before restore creates the destination VM or data disk.

Only locks that completed candidate qualification, immutable publication,
exact-URL re-download, and the isolated lifecycle are promoted for normal use.
Restore does not turn an unqualified candidate lock into a trusted release.

To move to a separately reviewed desired state during recovery, provide
`--lock PATH`. Hermes Box materializes and verifies every additional artifact
before creating destination resources.

After success, the destination creates its own Keychain-backed age identity.
Export it before making the first destination backup:

```bash
./bin/hermes-box --config /path/to/destination/hermes-box.yaml \
  key export /secure/path/destination-age-key.txt
./bin/hermes-box --config /path/to/destination/hermes-box.yaml \
  backup restored
```

The source identity is used only for decryption and is never silently imported.
Human authentication state on `/data` is restored; host browser sessions and
Keychain entries are not. A qualified cross-destination lifecycle uses a unique
temporary configuration and verifies this behavior before a root lock is
promoted.
