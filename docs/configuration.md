# Configuration

Fundarr works with no setting: run it, open it, create your account. Everything below is optional and set through environment variables.

## Data

`/data` holds `fundarr.db` and `secret.key`, the key that encrypts the stored API keys. **Back them up together**: without the key, the stored credentials cannot be read.

| Variable | |
|---|---|
| `FUNDARR_DATA` | Data directory (default `/data`). |
| `FUNDARR_ADDR` | Listen address (default `:9494`). |
| `FUNDARR_URL` | The address you open Fundarr at (`https://fundarr.example.com`). Needed for single sign-on, and shown to apps as the API address. |
| `FUNDARR_SECRET_KEY` | Master key in base64 (`openssl rand -base64 32`), instead of the key file. |
| `FUNDARR_DEMO` | `1` serves a made-up portfolio from memory, without sign-in or syncs, to try Fundarr or build an app. The data directory is not touched. |

## Sign-in

| | |
|---|---|
| **Username and password** (default) | Created in the UI on first launch, only from your local network or VPN. Change it in Settings. |
| **Single sign-on** | Any OpenID Connect provider (Authentik, Authelia, Keycloak, PocketID…): set `FUNDARR_OIDC_ISSUER`, `FUNDARR_OIDC_CLIENT_ID`, `FUNDARR_OIDC_CLIENT_SECRET`, `FUNDARR_URL`, and who may sign in with `FUNDARR_OIDC_ALLOWED_SUBS` or `FUNDARR_OIDC_ALLOWED_EMAILS` (comma-separated; emails must be verified). Redirect URI: `<FUNDARR_URL>/auth/callback`. |
| **Reverse proxy** | `FUNDARR_AUTH=proxy`: your proxy authenticates and passes the user in `Remote-User` (or `FUNDARR_PROXY_HEADER`). Fundarr must then only be reachable through that proxy. |
| **None** | `FUNDARR_AUTH=none`, for a machine only you can reach. |

| Variable | |
|---|---|
| `FUNDARR_OIDC_AUTO_REDIRECT` | `false` shows a Sign in button instead of going to the identity provider right away. |
| `FUNDARR_USERNAME`, `FUNDARR_PASSWORD_HASH` | Fix the credentials from the environment instead of the UI. Make the hash with `docker run --rm -it ghcr.io/alexis-marcel/fundarr hash-password`; in Compose, double every `$`. |

Apps reading the API use their own tokens (**Apps → Add app**), whatever the sign-in mode.

## Behind a reverse proxy

Fundarr listens on port 9494 and needs nothing special: forward the host to it. It does not need to be public; keep it behind your VPN or SSO.
