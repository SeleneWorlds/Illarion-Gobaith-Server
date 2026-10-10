# Illarion Gobaith Server on Selene

This Docker Compose setup runs Illarion Gobaith on Selene.

## Setup

1. Install Docker and Docker Compose.
2. Clone this repository and initialize the remaining bundle submodules:
   `git submodule update --init --recursive`.
3. Set up the two deploy keys below, copy `.env.example` to `.env`, and set the
   private keys and webhook secret.
4. Run `docker compose up --build -d`.

The Selene image compiles the player, admin, and editor UIs and includes the
remaining bundles and server configuration. The Go `repo-sync` sidecar clones
`illarion-gobaith-data` and `illarion-gobaith` into persistent volumes instead of
embedding them in the image. Existing local submodule checkouts are ignored and
are not imported into these volumes.

Selene waits until `GET /ready` reports that both repositories have successfully
cloned or pulled. Readiness is held in process memory and resets on every sidecar
restart. Initial failures keep it unready and retry every ten seconds.

## Deploy keys

Generate two separate, unencrypted SSH key pairs for unattended operation:

```sh
mkdir -p secrets
chmod 700 secrets
ssh-keygen -t ed25519 -N '' -C 'Selene Gobaith data' -f secrets/gobaith-data
ssh-keygen -t ed25519 -N '' -C 'Selene Gobaith scripts' -f secrets/gobaith-scripts
```

In each GitHub repository, open **Settings → Deploy keys → Add deploy key**:

- `Illarion-Gobaith-Data`: add `secrets/gobaith-data.pub` and enable **Allow write access**.
- `Illarion-Gobaith-Scripts`: add `secrets/gobaith-scripts.pub` with read-only access.

GitHub requires a separate deploy key for each repository. See
[GitHub's deploy key documentation](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/managing-deploy-keys).
Set `GOBAITH_DATA_DEPLOY_KEY` and `GOBAITH_SCRIPTS_DEPLOY_KEY` in your container's runtime
environment variables to the raw contents of the corresponding private key files.
Paste the complete multiline key, including the `BEGIN` and `END` lines, without
adding quotes. For a local `.env` file, enclose each multiline value
in single quotes. No encoding or file-secret support is required.
The variables are passed only to the sidecar, which writes them into temporary
files with mode `0600` and clears them from the environment before spawning Git.
It selects the corresponding key for each repository and disables agent identities
and interactive authentication. `secrets/` remains excluded from Git and the
Selene build context; no private keys are embedded in either image.

SSH verifies GitHub against the bundled, published Ed25519 host key using strict
host checking. The key comes from
[GitHub's SSH host key documentation](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints).
The webhook still uses its own shared secret; deploy keys only authenticate Git
operations.

## Data persistence

The sidecar commits all server edits in `illarion-gobaith-data` and pushes them
every five minutes. It also commits edits left by a previous run before the
startup pull. Failed commits or pushes are logged and retried; commits stay in
the existing `gobaith_data` volume. Pending commits are pushed even if no new
edits were made. The data repository's deploy key needs write access for pushing.

Configure these values in the ignored `.env` file:

| Variable | Purpose / default |
| --- | --- |
| `GOBAITH_DATA_DEPLOY_KEY` | Required complete multiline data private key |
| `GOBAITH_SCRIPTS_DEPLOY_KEY` | Required complete multiline scripts private key |
| `GOBAITH_DATA_GIT_AUTHOR_NAME` | Commit author; defaults to `Selene Server` |
| `GOBAITH_DATA_GIT_AUTHOR_EMAIL` | Commit email; defaults to `selene@localhost` |
| `GOBAITH_DATA_SYNC_INTERVAL_SECONDS` | Commit/push interval; defaults to `300`, range `1–86400` |
| `OPENAI_API_KEY` | Optional OpenAI API key for generated data commit messages |
| `OPENAI_MODEL` | Commit message model; defaults to `gpt-4.1-mini` |
| `GOBAITH_DATA_GIT_BRANCH` | Optional data branch; otherwise uses the remote default on first clone |
| `GOBAITH_SCRIPTS_GIT_BRANCH` | Optional scripts branch; otherwise uses the remote default on first clone |
| `GOBAITH_SCRIPTS_WEBHOOK_SECRET` | Shared GitHub webhook secret; empty disables webhooks |

When `OPENAI_API_KEY` is set, the sidecar sends staged file names (up to 16 KB)
and a staged diff (up to 64 KB) to the
[OpenAI Responses API](https://developers.openai.com/api/docs/guides/text)
to generate each data commit message, including startup commits. Requests disable
response storage and time out after 30 seconds. Without a key, or if generation
fails or returns an invalid message, it uses `Persist changes made by the Selene server`.
No request is made when there are no staged changes. The key is passed only to
the sidecar and cleared from its environment before Git subprocesses run.

The selected branches remain those of the persistent checkouts. Changing a branch
setting to differ from an existing checkout blocks readiness; migrate the checkout
explicitly rather than silently switching it. Scripts pulls use `--ff-only`, preserving local changes and refusing diverged
histories. Data sync fetches and rebases local commits onto the remote branch at
startup. After a failed data push, it fetches and rebases, then retries the push
once. Failed rebases are aborted to restore local commits and reported in the
logs; conflicts require manual resolution. The sidecar never force-pushes.

Remote data changes are also applied when recovering from a failed push, which
can update data files while Selene is running.
Background push failures do not clear startup readiness. Compose's readiness
dependency gates startup; it does not stop an already running Selene if the
sidecar later restarts or becomes unhealthy.

## Scripts webhook

`illarion-gobaith` uses the `SeleneWorlds/Illarion-Gobaith-Scripts` repository.
It is pulled at startup and after authenticated push webhooks. The sidecar never
commits or pushes this repository, and Selene mounts its checkout read-only.

The HTTP service listens on the Compose network. Expose the webhook through an
HTTPS reverse proxy on that network, forwarding to:

```text
http://repo-sync:8080/webhooks/illarion-gobaith
```

In the scripts repository's GitHub webhook settings, configure:

- **Payload URL:** your public HTTPS URL for `/webhooks/illarion-gobaith`.
- **Content type:** `application/json`.
- **Secret:** the value of `GOBAITH_SCRIPTS_WEBHOOK_SECRET`.
- **Events:** push events.

The handler validates `X-Hub-Signature-256` using HMAC-SHA256 and a constant-time
comparison, as described in [GitHub's webhook validation documentation](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries).
Only non-deletion pushes for the configured repository and checked-out branch
queue a pull. GitHub ping events receive `200`; valid queued pushes receive `202`;
irrelevant events receive `204`. Missing configuration or incomplete initial sync
returns `503`, invalid signatures return `401`, and invalid JSON returns `400`.

Pulls run asynchronously, coalesce bursts, and retry failures every ten seconds.
Git operations are serialized per repository, and webhook payloads cannot select
an arbitrary Git URL or command. Check completion with:

```sh
docker compose logs -f repo-sync
```

A successful pull updates the mounted script files. **Restart Selene to load
updated scripts:** `docker compose restart selene`. There is no automatic server
restart or reload. A script pull may update several files while Selene is running;
use a maintenance window if those files are read dynamically during gameplay.

## Updating

Update the remaining embedded submodules and rebuild:

```sh
git submodule sync --recursive
git submodule update --init --remote --recursive
docker compose up --build -d
```

To pull both runtime repositories and restart Selene without rebuilding it:

```sh
docker compose stop selene
docker compose up -d --force-recreate repo-sync selene
```

Do not remove volumes to perform an update.

## Sidecar development

The service uses the Go standard library and the Git CLI, with no Go dependencies.
From `docker/repo-sync`, run `go test -race ./...` and `go vet ./...`.
