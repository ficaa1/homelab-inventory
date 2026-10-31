# Application backup and migration

In Admin, enter a passphrase of at least twelve characters and choose **Download backup**. Use a strong, unique passphrase and store it separately. The `.hlb` file contains application credentials and private history. The passphrase cannot be recovered.

The API briefly drains HTTP operations and background workers, creates a consistent encrypted disk snapshot, then resumes the application before sending the download. Slow downloads do not keep maintenance active. Temporary files are private and removed after completion or cancellation.

Select the file and enter its passphrase, then choose **Preview restore**. Authentication and archive validation finish before any active dataset changes. The preview reports counts, byte size, source, prepared-plan digest, conversions and prerequisites. Private dashboard exports propose the source owner's Taste history in household profile `home`; optional fields change that mapping. Existing devices are not granted a new identity or request permission. Optional legacy agent compatibility authorizes the old dashboard key only for inventory posts, never administration, execution or signed release access.

Resolve any reported configuration or mount blockers and preview again. Type `REPLACE` and confirm to replace the whole dataset. This also replaces new history received after the preview. The staged plan expires after thirty minutes; Cancel discards it. Download a destination backup before replacing useful destination data.

Destination administrator, agent and operator credentials remain authoritative. Browser sessions and enrollment codes are invalidated. Destination browser identity settings, reverse proxy origin, execution policy and signing trust remain deployment decisions. Saved nullable settings and unknown plugin entries survive; the encrypted source configuration supplies their original fallback defaults.

Restore stops and joins background workers, installs the staged files, then constructs fresh stores, authentication, plugins and lazy TV managers before starting work again. It does not replay requests, actions, uploads or notification history. Legacy pending/unknown request receipts remain a duplicate barrier. External services retain their own state.

## External prerequisites

This archive covers the configured API data directory. It does not traverse external paths: service databases, media libraries, upload content, worker state, agent installations, signed release trees, signer keys, compose files, TLS/reverse proxy configuration and host permissions need separate backups and destination mounts. Recognized source connection settings travel encrypted. Deployment/signing files named `.env`, `.env.*`, `*.key`, `*.pem`, `id_rsa`, `id_ed25519`, or common compose filenames are excluded even if placed under the data directory. Symlinks and non-regular files are refused rather than followed.

Optional plugin conversions enable the integrations listed in the preview. Polling/synchronization resumes only after the replacement validates. No restore step sends an infrastructure action or retries a media request. Upload folder mounts and provider connectivity must still be available on the destination.

## Recovery and limits

The volume root itself is never renamed. A durable `.backup-journal.json` records child-file replacement and keeps the old generation until the new generation validates. Startup recovers an interrupted transaction before opening any managers. Do not manually remove the journal or rollback directory during a recovery.

If restore reports an error, inspect server health and logs before retrying. An error after the commit marker can mean the new generation is installed but cleanup failed; automatic recovery protects the last committed dataset. Stop the API before operator filesystem recovery, retain the journal and rollback files, and keep the encrypted backup.

Default limits are 1,000,000 files, 8 GiB per file and 256 GiB total expanded content. Deployment variables `BACKUP_MAX_FILES`, `BACKUP_MAX_FILE_BYTES` and `BACKUP_MAX_TOTAL_BYTES` may raise bounds for large histories. Staging requires space for the encrypted upload and expanded dataset while the old dataset still exists; download requires room for its encrypted snapshot. Limits and available filesystem capacity should be sized before a large migration.

## File format

Envelope version 1 authenticates the entire header and ciphertext before archive parsing: PBKDF2-HMAC-SHA256 with 600,000 iterations and a random 32-byte salt derives separate AES-256-CTR and HMAC-SHA256 keys; a random 16-byte IV drives encryption. HMAC covers the version, parameters, salt, IV and ciphertext. The trailing 32-byte tag is checked with constant-time comparison on a bounded private disk spool before any plaintext is exposed.

Inside is a versioned gzip/tar archive with a manifest containing product/build/schema, timestamp, allowlisted configuration, exclusions, and path/length/SHA-256 for every file. Extraction rejects unsupported versions, traversal, absolute/backslash paths, duplicate entries, links, devices, hash/length mismatches and decompression excess. Prepared conversion bytes are sealed into the exact digest accepted by confirmation. Inventory and software histories preserve original bytes, including unknown additive schema fields.
