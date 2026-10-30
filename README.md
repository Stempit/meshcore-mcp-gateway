# meshcore-mcp-gateway 🎙️📡

**An MCP gateway to your mesh radio. Talk to it. Literally.**

meshcore-mcp-gateway connects AI agents to a MeshCore LoRa radio over MCP
(Model Context Protocol). Ask the [XiaoZhi](https://xiaozhi.me) speaker on
your desk *"are there new messages on the mesh?"* and it reads them out; say
*"reply to Wire: on my way"* and it goes out over the radio. The same tools
work for any MCP host — [nanobot](https://github.com/nanobot-ai/nanobot),
[Hermes Agent](https://github.com/NousResearch/hermes-agent), Claude, your
own agent — from one gateway, one radio.

It is a **standalone feature**: the gateway opens its own connection to the
MeshCore device (BLE, USB serial or TCP over Wi-Fi/Ethernet, the `Connection`
config section) and
needs nothing else — not the HTTP gateway, not the bot.

## Transports

One process serves any mix of three MCP transports:

| Transport | Direction | For |
|---|---|---|
| `MCP` (xiaozhi.me) | dials **out** over WebSocket | XiaoZhi voice assistants via the MCP access point |
| `Transports.http` | inbound Streamable HTTP | nanobot, Hermes, any MCP host that takes a URL |
| `Transports.stdio` / `-stdio` | stdin/stdout | hosts that spawn the gateway as a subprocess |

The XiaoZhi side is the original trick — the direction of the connection:

```
[XiaoZhi device] ⇄ ws ⇄ [xiaozhi.me cloud] ⇐= outbound wss =⇐ [meshcore-mcp-gateway]
                                                                ⇅ BLE/serial
                                                          [MeshCore device]
```

The gateway **dials out** to xiaozhi.me and acts as an MCP *server* on that
connection — the cloud is the MCP client. Because the connection is outbound,
it works behind ISP NAT, CGNAT, firewalls: no port forwarding, no public IP,
no tunnel. (This is the same access-point mechanism XiaoZhi-based
integrations use.)

Generic agents connect the usual way instead — gateway as a server:

```
[nanobot / hermes / any MCP host] ⇒ HTTP POST /mcp ⇒ [meshcore-mcp-gateway]
                                                        ⇅ BLE/serial
                                                  [MeshCore device]
```

Every connected client gets its **own read cursor**, keyed by the client name
it announces in `initialize`: hermes draining the inbox does not hide
messages from XiaoZhi, and a reconnecting client resumes where it left off
instead of replaying. Sends share one radio and are serialized by the device
driver.

*[🇷🇺 Русская версия](README_RU.md)*

---

## What the agent can do

| Tool | Voice example | What it does |
|---|---|---|
| `mesh_get_new_messages` | *"прочитай новые сообщения"* | Reads unread mesh messages (10 per call, pages through) |
| `mesh_send_message` | *"отправь Короеду: буду через час"* | Sends a dm or channel message over the radio |
| `mesh_search_contacts` | *"найди контакта Йорика"* | Searches contacts by a name fragment (case/emoji ignored) — how the agent finds recipients in a large contact book |
| `mesh_list_contacts` | *"кто есть в сети?"* | Lists the first known node names — fine for small contact lists |
| `mesh_list_channels` | *"какие каналы есть?"* | Lists configured channels |

Tool failures come back as readable results ("recipient not found — use
mesh_list_contacts to check names") so the agent can tell you what went wrong
instead of dying silently.

## Setup

**1. The device.** Configure how to reach the MeshCore companion (shared by
all transports):

```yaml
# config.yaml (subset of the family format; unknown sections are ignored)
Connection:
  type: "bt"             # "bt" (BLE, default), "serial" or "tcp" (Wi-Fi/Ethernet)
  # serial_port: "/dev/ttyUSB0"
  # serial_baud: 115200
  # tcp_address: "192.168.1.50"   # when type: "tcp" — host or host:port (default 5000)
  # device_name: "MeshCore-AB12"  # when type: "bt" — pick one device by advertised name
  # device_address: "AA:BB:CC:DD:EE:FF"  # when type: "bt" — or by platform address
  channel_transmissions: 3

Shrink:                  # how outgoing messages are fitted to the mesh
  cyr_to_lat: true       # squeeze Cyrillic to identical-looking Latin (saves bytes)
  # max_bytes: 140       # hard mesh limit
  # overrides:
  #   xiaozhi: split     # voice path defaults to split; override if you like
```

**2. At least one transport** (see below). With none configured the gateway
refuses to start.

**3. Run:** `go run .` (or the binary; `-config` picks a different config
file).

### Transport: XiaoZhi

Get your MCP access point URL: log in to the [xiaozhi.me
console](https://xiaozhi.me) → open your agent (智能体) → find **MCP 设置 /
获取 MCP 接入点** and copy the endpoint URL
(`https://api.xiaozhi.me/mcp/?token=…`). It is per-agent: the tools appear on
that one agent.

```yaml
MCP:
  endpoint_url: "https://api.xiaozhi.me/mcp/?token=XXXX"
```

The console's MCP status indicator should flip to connected within seconds.
Now just talk to your XiaoZhi device.

### Transport: HTTP — nanobot, Hermes, any MCP host

```yaml
Transports:
  http:
    enabled: true
    # listen: "127.0.0.1:8788"   # default; loopback needs no token
    # path: "/mcp"               # default
    # token: "shared-secret"     # required when listen is not loopback
```

Then attach the gateway in your host's config:

```yaml
# Hermes Agent (~/.hermes/config.yaml; see the Hermes MCP docs for the
# exact keys of your version):
mcp_servers:
  meshcore:
    url: "http://127.0.0.1:8788/mcp"
    headers:
      Authorization: "Bearer shared-secret"
```

```yaml
# nanobot (nanobot.yaml):
mcpServers:
  meshcore:
    url: "http://127.0.0.1:8788/mcp"
    headers:
      Authorization: "Bearer ${MESHCORE_TOKEN}"
```

Multiple hosts can be connected at once — each keeps its own message cursor.

**Exposing beyond localhost:** a non-loopback `listen` without a `token` is
rejected at startup. There is no built-in TLS — put the gateway behind
Tailscale, an SSH tunnel, or a reverse proxy when reaching it over a network.

### Transport: stdio

For hosts that launch the gateway as a subprocess (stdio MCP server):

```json
{
  "command": "/path/to/meshcore-mcp-gateway",
  "args": ["-config", "/path/to/config.yaml", "-stdio"]
}
```

Frames are newline-delimited JSON-RPC on stdin/stdout; logs go to stderr.
The `-stdio` flag can also be combined with other transports. Mind the
one-device rule below: don't let every agent spawn its own copy against the
same radio — prefer one gateway with HTTP and point the agents at it.

### Config reference

| Key | Default | Meaning |
|---|---|---|
| `MCP.endpoint_url` | *(optional)* | xiaozhi.me MCP access point URL (dial-out WebSocket) |
| `Transports.http.enabled` | `false` | Serve Streamable HTTP for generic MCP hosts |
| `Transports.http.listen` | `127.0.0.1:8788` | Bind address; non-loopback requires `token` |
| `Transports.http.path` | `/mcp` | MCP endpoint path |
| `Transports.http.token` | — | Bearer token clients must present |
| `Transports.stdio.enabled` | `false` | Serve newline-delimited JSON-RPC on stdin/stdout (also: `-stdio` flag) |
| `Output.max_message_runes` | `200` | Per-message truncation in `mesh_get_new_messages` — raise for text agents |
| `Connection.*` | — | Which MeshCore device to attach to, and how — BLE (with optional name/address selection), USB serial or TCP |
| `Shrink.*` | voice path defaults to `split` | Outgoing-message fitting — see the behavior notes below |

Flags: `-config <path>` (default `./config.yaml`), `-stdio`.

### One app per device

The gateway attaches to the companion device directly. Don't run it against
the **same physical device** as [meshcore-go-bot](https://github.com/stempit/meshcore-go-bot)
or the [HTTP gateway](https://github.com/stempit/meshcore-http-gateway) at
the same time — a MeshCore companion serves one BLE/serial connection.
Different devices are fine. Within one gateway process, any number of agents
can share the radio — that's the point.

## Behavior details

- **Russian finds English names.** Contact matching is phonetic and
  language-agnostic: query *"Йорик"* and both search and send find
  `Yorick 💀` (and vice versa — a Latin-spelled query finds `Короед`).
  Matching first tries the exact stored name, then ignores case, emoji and
  punctuation, then falls back to a phonetic fuzzy match with a small edit
  budget. If several contacts match equally well, the agent gets the
  candidate list and asks for the full name.
- **Sends are mesh-fitted.** Outgoing messages pass through the same `fit`
  package (from [meshcore-kit](https://github.com/stempit/meshcore-kit)) as
  every bot send. By default a message longer than the mesh limit is
  **split** into sequenced `[i/N]` parts, sent one after another (each takes
  up to ~30 s of radio time). With `Shrink.cyr_to_lat: true` Cyrillic is
  first squeezed to identical-looking Latin letters, buying extra bytes.
  Override the strategy with `Shrink.overrides.xiaozhi` (`truncate`,
  `split`, `skip`).
- **Per-client cursors.** On its first connect a client's cursor is primed
  to the newest inbox ID — "new messages" means *arrived from now on*; the
  backlog that existed before the client connected is not replayed. The
  cursor survives reconnects (keyed by the `clientInfo.name` the client
  announced); restart the gateway to start everyone fresh.
- **Slow sends are normal.** Radio delivery confirmation takes up to ~30 s;
  `mesh_send_message` blocks that long and the agent pauses before
  confirming.
- **504 ≠ lost.** A send that times out without radio confirmation is
  reported as "transmitted but not confirmed — it may still arrive"; don't
  repeat it blindly, radio retries may still land it.
- **Automatic reconnect**, both directions: to xiaozhi.me (exponential
  backoff, 2 s → 60 s) and to the BLE device (periodic stats watchdog).
  The xiaozhi.me link is kept alive with WebSocket pings every 30 s — idle
  gaps between voice commands would otherwise get the connection killed by
  NAT or load-balancer idle timeouts (close 1006), and a silent link is
  dropped after 90 s without a pong.

## Source map

`main.go` (composition root + config), `gateway/server.go` (JSON-RPC/MCP
wire handling + tool implementations, per-client `Conn`), `gateway/xiaozhi.go`
(outbound WebSocket + reconnect), `gateway/http.go` (Streamable HTTP
transport), `gateway/stdio.go` (stdio transport), `gateway/mesh.go` (`Mesh`
interface + direct device backend), `gateway/tools.go` (tool descriptors).
The device runtime (connect, startup sync, inbox, keepalive) comes from
[meshcore-kit](https://github.com/stempit/meshcore-kit); the protocol driver
from [meshcore-go](https://github.com/stempit/meshcore-go).

## License

MIT — see [LICENSE](LICENSE).
