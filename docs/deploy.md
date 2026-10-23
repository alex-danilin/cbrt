# Deploying cbrt

cbrt is one static binary with no runtime dependencies. You can run it as a plain process, a systemd service, a Windows scheduled task, or a container. Whichever you choose, the security rule is the same (see [Security](gateway.md#security)):

- **Loopback (`127.0.0.1`) is the default** and needs nothing else.
- **Any other bind address** needs `api_key` (`${CBRT_API_KEY}`) **and** either native TLS (`tls_cert`/`tls_key`) or `trust_proxy: true` behind a TLS reverse proxy. cbrt refuses to start otherwise.
- cbrt makes **no outbound connections** except to the providers you configure. With `"offline": true` it doesn't contact cloud providers at all, including for model discovery.
- There is no SIGHUP handler. To reload `cbrt.json` and the `-env-file` after editing them, call `POST /api/reload` with the API key.
  - Edited or removed `.env` entries take effect. Variables set in the real process environment (including Docker `env_file`) always win over the file.
  - A reload that changes `listen`, `tls_cert`, `tls_key`, `data_dir`, `retention_months`, `limits.max_concurrent_requests`, `limits.max_concurrent_streams` or `limits.requests_per_minute` is refused with 400 and nothing is applied. Those need a restart.
  - A config that fails validation is refused with 400 as well; the running configuration stays in place.
  - Changing `api_key` signs out all dashboard sessions.

Files in this repo:

| File | Purpose |
|---|---|
| `Dockerfile` | Multi-stage build that produces a `FROM scratch` image (~12 MB) running as uid 65532 |
| `docker-compose.yml` | cbrt plus a bundled Ollama on an internal-only network |
| `deploy/cbrt.docker.json` | The config used inside containers |
| `deploy/cbrt.service` | A hardened systemd unit |

## Docker

### The image

```sh
docker build -t cbrt .
```

- **Build stage:** `golang:1.26-alpine` runs `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`. Only `go.mod`, `cmd/` and `internal/` go into the build context (`.dockerignore` is an allow-list), and the dashboard HTML is embedded into the binary. `.env`, `cbrt.json`, `data/` and `captures/` never enter the build context, so **the image contains no secrets**.
- **Final stage (`scratch`):** contains `/cbrt`, CA roots (needed for HTTPS to NVIDIA), an `/etc/passwd` entry for `cbrt` (uid/gid 65532), and an empty `/data` owned by 65532. It runs as `USER 65532:65532`.
- **Command:**
  - The entrypoint is `/cbrt` and the default command is `serve -config /etc/cbrt/cbrt.json -env-file /etc/cbrt/.env`.
  - You don't need to mount `/etc/cbrt/.env`: environment variables work too, and cbrt ignores a missing env file.
  - Mount the env file (rather than passing it as container environment) if you want `POST /api/reload` to pick up rotated keys without a restart.
- **No shell and no curl.** That's why the image has no `HEALTHCHECK`. Point your orchestrator's HTTP probe at `GET /healthz` instead, which returns `200 ok` without auth. Examples: a Kubernetes `httpGet` liveness/readiness probe, a load-balancer health check, Caddy `health_uri /healthz`, or a host cron running `curl -fsS http://127.0.0.1:8080/healthz`.
- **Writable paths:** only `/data` is written to (JSONL telemetry plus `aggregates.json`, including its temp file), so the container runs fine with `--read-only`.

### The container config: `deploy/cbrt.docker.json`

A listener on `127.0.0.1` inside a container can't be reached through a published port, so this config sets:

```json
"listen": "0.0.0.0:8080",
"api_key": "${CBRT_API_KEY}",
"trust_proxy": true,
"trusted_proxies": ["172.16.0.0/12"],
"data_dir": "/data",
```

`trust_proxy: true` tells cbrt that **something else terminates TLS**. You have to make that true. Do one of these:

1. **Publish on host loopback only:** `-p 127.0.0.1:8080:8080`. The compose file does this. Only processes on the host can connect, as with a native loopback install.
2. **Put a TLS reverse proxy in front** and publish nothing else ([Caddy example](#reverse-proxy-caddy) below).

Never publish port 8080 on a public interface as plain HTTP: the API key would travel in cleartext.

`trusted_proxies` lists the peers whose `X-Forwarded-For` header cbrt believes (see [Client addresses behind a proxy](#client-addresses-behind-a-proxy)). `172.16.0.0/12` is Docker's default bridge address range. Both proxy setups above reach the container from it: a Caddy container on the compose network connects from its own bridge address, and a proxy on the host that connects to the published `127.0.0.1:8080` arrives through Docker's port proxy from the bridge gateway (`172.17.0.1`-style). If your daemon uses other address pools (`default-address-pools` in `daemon.json`; check with `docker network inspect`), put that range here instead. A client that reaches a published port directly keeps its own source address. That address isn't in the list, so the client can't choose the address its login attempts are counted under.

Providers:
- **`nvidia`:** `https://integrate.api.nvidia.com/v1` with `${NVIDIA_API_KEY}`.
- **`ollama`:** `base_url` is `${OLLAMA_BASE_URL}`. `${VAR}` expansion works in `listen`, the top-level `api_key`, `tls_cert`/`tls_key`, `data_dir`, and each provider's `base_url` and `api_key`. It doesn't work in booleans such as `offline`.
  - Compose sets `http://ollama:11434/v1`, the bundled service.
  - For Ollama running natively on the Docker host, use `http://host.docker.internal:11434/v1`. Docker Desktop resolves this name by default. On Linux, add `--add-host host.docker.internal:host-gateway` (compose already does), and make host Ollama listen on an address the bridge can reach (`OLLAMA_HOST=0.0.0.0` or the docker0 IP, firewalled).
  - **Always set `OLLAMA_BASE_URL`.** If it's empty, cbrt silently falls back to `http://127.0.0.1:11434/v1`, which is the container itself.

The routes match `cbrt.example.json` (`default`, `deep`, `vision`) plus `claude-*` for Claude Code and `coder` for Codex. The Ollama model names (`gemma-4-e2b-4-32k`, `minicpm-v4.6-32k`) are local 32K-context variants. Create them in whichever Ollama you use ([Ollama models](#ollama-models-and-context)) or edit the names.

### Plain `docker run`

```sh
docker run -d --name cbrt --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD/deploy/cbrt.docker.json:/etc/cbrt/cbrt.json:ro" \
  -v cbrt-data:/data \
  --env-file .env \
  -e OLLAMA_BASE_URL=http://host.docker.internal:11434/v1 \
  --add-host host.docker.internal:host-gateway \
  cbrt
```

`.env` must contain `CBRT_API_KEY=<long random string>` (e.g. `openssl rand -hex 24`) and `NVIDIA_API_KEY=nvapi-…`.

### Compose

```sh
printf 'CBRT_API_KEY=%s\nNVIDIA_API_KEY=nvapi-...\n' "$(openssl rand -hex 24)" > .env
docker compose up -d --build
curl -fsS http://127.0.0.1:8080/healthz
```

- **`cbrt` service:**
  - Builds from `.`.
  - Publishes `127.0.0.1:${CBRT_PORT:-8080}:8080`.
  - Mounts the config read-only and keeps data in the named volume `cbrt-data`.
  - Reads secrets from `.env` via `env_file`, at run time only.
  - Hardening: `read_only: true`, `cap_drop: [ALL]`, `no-new-privileges`, `mem_limit: 256m`, `pids_limit: 256`.
  - Restart policy: `unless-stopped`.
- **`ollama` service:**
  - Uses `ollama/ollama` with models in the named volume `cbrt-ollama-models` and `OLLAMA_CONTEXT_LENGTH=32768`.
  - Sits only on the `backend` network, which is `internal: true`: port 11434 isn't published to the host, and Ollama has **no internet egress**. That also rules out its auto-update and any accidental model pulls.
  - Load models ahead of time, as described below. A commented `deploy.resources` block enables NVIDIA GPUs.
- **Using Ollama already running on the host:** don't start the bundled one.

  ```sh
  echo OLLAMA_BASE_URL=http://host.docker.internal:11434/v1 >> .env
  docker compose up -d --build --no-deps cbrt
  ```

- **Changing the config:** edit `deploy/cbrt.docker.json`, then run

  ```sh
  curl -fsS -X POST -H "Authorization: Bearer $CBRT_API_KEY" http://127.0.0.1:8080/api/reload
  ```

  Keys that come from `env_file` are fixed for the container's lifetime, so after rotating them run `docker compose up -d` to recreate the container.

### Ollama models and context

Ollama defaults to a **4096-token context**, and agents need far more (Claude Code's first request is about 30K tokens). There are two ways to raise it:

- **Per server:** `OLLAMA_CONTEXT_LENGTH=32768`, as the compose file sets. Plain model tags then get 32K, and the route's `max_context` should say `32768`.
- **Per model:** a Modelfile variant, which is how the `-32k` names in the config are made:

  ```sh
  docker compose exec ollama sh -c 'printf "FROM minicpm-v4.6\nPARAMETER num_ctx 32768\n" > /tmp/Mf && ollama create minicpm-v4.6-32k -f /tmp/Mf'
  ```

A larger context uses more RAM or VRAM, so size the host accordingly.

## Fully offline deployment

Do everything that needs the internet on an online machine, then move the artifacts.

1. **Build and pull while online:**

   ```sh
   docker compose build                 # cbrt:latest
   docker pull ollama/ollama:<version>  # pin the tag in docker-compose.yml too
   ```

2. **Pre-pull models into a volume.** The compose Ollama has no egress, so use a temporary online container on the same named volume:

   ```sh
   docker volume create cbrt-ollama-models
   docker run -d --name ollama-seed -v cbrt-ollama-models:/root/.ollama ollama/ollama:<version>
   docker exec ollama-seed ollama pull minicpm-v4.6
   docker exec ollama-seed ollama pull <other-model>
   docker exec ollama-seed sh -c 'printf "FROM minicpm-v4.6\nPARAMETER num_ctx 32768\n" > /tmp/Mf && ollama create minicpm-v4.6-32k -f /tmp/Mf'
   docker rm -f ollama-seed
   ```

   `docker compose run ollama pull …` doesn't work here: `ollama pull` needs a running server, and the compose network is internal. Instead of pulling, you can also copy an existing models directory (`~/.ollama/models`, or `%USERPROFILE%\.ollama\models` on Windows) into the volume.

3. **Move everything to the air-gapped host:**

   ```sh
   docker save cbrt:latest ollama/ollama:<version> | gzip > cbrt-images.tar.gz
   docker run --rm -v cbrt-ollama-models:/v -v "$PWD":/out alpine tar czf /out/ollama-models.tgz -C /v .
   # copy both files, docker-compose.yml and deploy/ to the offline host, then:
   docker load < cbrt-images.tar.gz
   docker volume create cbrt-ollama-models
   docker run --rm -v cbrt-ollama-models:/v -v "$PWD":/in alpine tar xzf /in/ollama-models.tgz -C /v
   ```

   (Also `docker save` the `alpine` image, or use any image with `tar`.)

4. **Configure offline mode:**
   - Set `"offline": true` in `deploy/cbrt.docker.json`. Cloud providers are then disabled, including discovery, and `NVIDIA_API_KEY` is no longer required.
   - Remove cloud targets from routes, or leave them; they're skipped.
   - Keep `OLLAMA_CONTEXT_LENGTH` set.
5. **Start without building:** `docker compose up -d --no-build`.

Nothing else phones home. cbrt contacts only the configured providers, the dashboard is embedded with no CDN, and telemetry stays in `/data`. For a native (non-Docker) Ollama on an offline host, also turn off the Ollama app's auto-update, which checks GitHub hourly.

## Reverse proxy (Caddy)

Use this for remote access with automatic TLS. cbrt stays on loopback (`127.0.0.1:8080`, either native or the compose port) with `trust_proxy: true` and an `api_key`.

```caddyfile
llm.example.com {
	encode zstd gzip
	reverse_proxy 127.0.0.1:8080 {
		flush_interval -1          # stream SSE tokens immediately
		transport http {
			read_timeout 15m       # long agent turns / slow first byte on free tiers
		}
		health_uri /healthz
	}
}
```

- **What Caddy does:** it sends `X-Forwarded-Proto: https`, which cbrt uses to mark the dashboard session cookie `Secure`.
- **Host allow-list:** add `"allowed_hosts": ["llm.example.com"]` to the config so requests with other `Host` headers are refused. It's off unless set.
- **Caddy in compose:** put it on the `default` network, point it at `cbrt:8080`, and remove the `ports:` mapping from `cbrt`.

### Client addresses behind a proxy

The dashboard login is limited to 5 attempts per minute per client address (IPv6 clients are grouped by `/64`). On top of that, at most 30 wrong keys per minute are accepted from all clients combined. Once that cap is reached, every sign-in is refused until a minute has passed. API requests with the key in a header aren't affected.

Behind a proxy every connection comes from the proxy, so cbrt takes the client address from `X-Forwarded-For`. It only does so when the connection itself comes from an address in `trusted_proxies`; from any other peer the header is ignored and the peer's own address counts. cbrt reads the header from the right and skips trusted hops, so entries a client wrote itself are never used.

- **Default:** with `trust_proxy: true` and no `trusted_proxies`, only loopback (`127.0.0.0/8`, `::1`) is trusted. That fits Caddy or nginx on the same host.
- **Proxy on another machine or network:** list its address or range, e.g. `"trusted_proxies": ["10.0.5.2", "fd00:5::/64"]`. Entries are IPs or CIDRs, and the list replaces the loopback default.
- **Keep the list narrow.** Any peer in it can claim any client address. Docker's port proxy, for example, hands connections over from the bridge gateway, so a trusted bridge range also covers anything that port proxy forwards. Don't publish the port publicly with such a range in the list. The global failure cap still bounds guessing if a range is too wide.
- **Upgrading:** earlier versions believed `X-Forwarded-For` from any peer when `trust_proxy` was on. A proxy that isn't on loopback now needs an entry in `trusted_proxies`. Until you add one, all clients behind it share one login limit, the proxy's.

## Linux: systemd

`deploy/cbrt.service` runs the release binary as a dedicated system user:

```sh
useradd --system --no-create-home --home-dir /var/lib/cbrt --shell /usr/sbin/nologin cbrt
install -m 0755 dist/cbrt-linux-amd64 /usr/local/bin/cbrt
install -d -m 0750 -o root -g cbrt /etc/cbrt
install -m 0640 -o root -g cbrt cbrt.json /etc/cbrt/cbrt.json   # set "data_dir": "/var/lib/cbrt"
install -m 0640 -o root -g cbrt .env /etc/cbrt/.env
install -m 0644 deploy/cbrt.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now cbrt
journalctl -u cbrt -f
```

- **Command:** `ExecStart=/usr/local/bin/cbrt serve -config /etc/cbrt/cbrt.json -env-file /etc/cbrt/.env`, with `Restart=on-failure`.
- **Data:** `StateDirectory=cbrt`, so data lives in `/var/lib/cbrt`. That is the only path the service can write (`ReadWritePaths`).
- **Hardening:**
  - `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `PrivateDevices`;
  - the kernel/cgroup/clock protections;
  - `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`;
  - an empty capability set;
  - `SystemCallFilter=@system-service` (denied calls return `EPERM`);
  - `MemoryDenyWriteExecute`;
  - `MemoryMax=512M`.
  - Check the result with `systemd-analyze security cbrt`.
- **DynamicUser:** the unit's header shows the alternative, `DynamicUser=yes` plus `LoadCredential=env:/etc/cbrt/.env` and `-env-file %d/env`, for hosts where you don't want a static account.
- **Reload:** there is no `ExecReload` because cbrt has no SIGHUP handler. Use `curl -X POST -H "Authorization: Bearer …" http://127.0.0.1:8080/api/reload`.
- **Remote access:** keep `listen` on `127.0.0.1:8080` and add Caddy/nginx with TLS and `trust_proxy: true`, or set native `tls_cert`/`tls_key`. To bind :443 directly, grant `CAP_NET_BIND_SERVICE` as the comment in the unit shows.

## Windows

- **Running it:** run the binary directly, e.g. `dist\cbrt-windows-amd64.exe serve -config cbrt.json -env-file .env`. It listens on `127.0.0.1:8080`, and data goes to `data\` next to the working directory.
- **Starting at boot:** there is no built-in service mode. Choose one:
  - **Task Scheduler.** Create a task with the trigger *At startup* (or *At log on*) and the action `cbrt.exe serve -config C:\cbrt\cbrt.json -env-file C:\cbrt\.env` with *Start in* `C:\cbrt`. Select *Run whether user is logged on or not*, and on the Settings tab clear *Stop the task if it runs longer than…*. Or from an elevated prompt:

    ```bat
    schtasks /Create /TN cbrt /SC ONSTART /RU "NT AUTHORITY\LOCAL SERVICE" /TR "C:\cbrt\cbrt.exe serve -config C:\cbrt\cbrt.json -env-file C:\cbrt\.env"
    ```

    Give the account write access to `C:\cbrt\data` and read access to the config.
  - **A service wrapper.** Wrappers in the NSSM/WinSW style turn any console program into a Windows service with restart-on-failure and log capture. Point the wrapper at `cbrt.exe` with the same arguments and working directory.
- **Reload:** `curl.exe -X POST -H "Authorization: Bearer %CBRT_API_KEY%" http://127.0.0.1:8080/api/reload`. Windows has no SIGHUP anyway.
- **Smart App Control / SmartScreen:** these can block unsigned binaries such as `cbrt.exe` (and Go test binaries). Use a signed build, run it inside WSL or Docker Desktop, or adjust the policy on machines you control.
- **Docker Desktop:** the compose file works unchanged. `host.docker.internal` reaches native Windows Ollama at `127.0.0.1:11434` without further setup.

## Cloud VM

- **Recommended setup:** keep `listen: "127.0.0.1:8080"` (native) or publish `127.0.0.1:8080:8080` (Docker). Run Caddy/nginx on 443 with a real certificate, set `trust_proxy: true`, `api_key`, and `allowed_hosts: ["your.domain"]`. Open only 443 (and 80 for ACME) in the firewall or security group.
- **Without a proxy:** set `listen: "0.0.0.0:8443"`, `tls_cert` and `tls_key` (PEM files readable by the service user), plus `api_key`. cbrt serves HTTPS itself. Renew certificates externally and restart.
- **Dashboard:** it is protected by the same API key, via a login form that sets an HttpOnly, SameSite=Strict cookie.
- **Built-in limits:** 32 concurrent requests, 16 streams, 60 requests/min and a 64 MB in-flight budget. Tune them under `limits` for shared use.
- **Keys:** use a long random `CBRT_API_KEY` and rotate it by editing `.env` and calling `/api/reload`, or restarting under Docker `env_file`.

## Pointing Claude Code and Codex at a remote gateway

Replace `https://llm.example.com` with your proxy URL (or `http://127.0.0.1:8080` over an SSH tunnel: `ssh -N -L 8080:127.0.0.1:8080 vm`).

**Claude Code** (`~/.claude/settings.json`):

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://llm.example.com",
    "ANTHROPIC_AUTH_TOKEN": "<CBRT_API_KEY>",
    "CLAUDE_CODE_MAX_CONTEXT_TOKENS": "32768"
  },
  "statusLine": { "type": "command", "command": "cbrt statusline -url https://llm.example.com" }
}
```

- Leave `ANTHROPIC_API_KEY` unset.
- The status line reads the key from `-key`, `CBRT_API_KEY` or `ANTHROPIC_AUTH_TOKEN`.
- `claude-*` requests hit the `claude-*` route.

**Codex** (`~/.codex/config.toml`), with `export CBRT_API_KEY=…` in the shell:

```toml
[model_providers.cbrt]
name = "cbrt"
base_url = "https://llm.example.com/v1"
wire_api = "responses"
env_key = "CBRT_API_KEY"

[profiles.free]
model_provider = "cbrt"
model = "coder"
model_context_window = 32768
```

**Any OpenAI-compatible client:** set the base URL to `https://llm.example.com/v1` and send the header `Authorization: Bearer $CBRT_API_KEY` (or `x-api-key`).

## Verified

On Docker 29.2 (Docker Desktop, Windows 11):
- `docker build -t cbrt:test .` produced an image of **12.1 MB** (3.6 MB compressed).
- With `deploy/cbrt.docker.json` mounted, `--read-only --cap-drop ALL`:
  - `/healthz` returned 200 and `/api/stats` returned 200 with the key and 401 without.
  - `docker top` showed uid 65532.
  - Discovery reached NVIDIA over HTTPS.
  - A chat completion to `ollama:minicpm-v4.6-32k` on host Ollama via `host.docker.internal` succeeded.
  - Telemetry landed only in the `/data` volume.
- `docker compose config` validated. The compose `cbrt` service with host Ollama (`--no-deps`) also passed `/healthz` and discovery. The bundled `ollama` service wasn't started in that test.
