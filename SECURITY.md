# Security

## Private data

Do not commit, publish, or paste any of the following into issues or logs:

- `.prepare/marvis.json`, upstream signing keys, registered device ids, and client caches.
- Account access / refresh tokens, Cookies, keychain exports, or files from `data/`, `auths/`, `accounts/`, and backups.
- Panel passwords, API keys, `.env`, local configuration, packet captures, or screenshots exposing these values.

`.gitignore` excludes normal runtime paths. Docker uses an allowlisted build context and loads the prepare file only through a read-only runtime mount. Neither rule protects a file forcibly added to Git or exported to another location. Review staged changes and history before making a repository public.

CI masks and removes email fields from the event payload before Docker actions run. Automatic Docker build-record uploads and provenance attestations are disabled because they can include event metadata such as private push-account emails. Check Actions logs, build records, image configuration, and attestation metadata as well as the image filesystem when reviewing a release.

The prepare directory is `0700`; the file is `0644` so a non-root container UID can read its single-file mount. Host root and Docker administrators can still access it. Do not copy it into a shared directory. Runtime data is not encrypted by this project; secure backups separately.

## Deployment

- The panel default password is `marvis`. Change it immediately; it is not an API key.
- Compose binds only to `127.0.0.1` by default. Use SSH forwarding or authenticated HTTPS access rather than exposing a default installation.
- Only use accounts and client files you are authorized to use. Upstream protocol changes or risk controls may invalidate credentials or restrict accounts.
- A syntactically valid device id is not proof of successful upstream registration or accepted cross-machine use.

## Reporting vulnerabilities

Use [GitHub private vulnerability reporting](https://github.com/qianjindexiaozu/marvis2api-panel/security/advisories/new) when enabled. If it is unavailable, contact the repository owner privately; do not put exploit details or credentials into a public issue.

Include the affected version, deployment mode, a minimal reproduction using fake data, and the expected impact. Remove all signing values, device ids, tokens, and API keys. Do not test against accounts or services you are not authorized to use.

An upstream application-wide signing key is not an account token, but it still must not be distributed in this repository or image. Moving it to configuration does not itself establish authorization to use the upstream service.
