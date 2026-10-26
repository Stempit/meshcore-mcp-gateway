# meshcore-mcp-gateway 🎙️📡

**Talk to your mesh radio. Literally.**

meshcore-mcp-gateway connects the [XiaoZhi](https://xiaozhi.me) AI voice
assistant to a MeshCore LoRa radio. Ask the speaker on your desk *"are there
new messages on the mesh?"* and it reads them out; say *"reply to Wire: on my
way"* and it goes out over the radio.

It is a **standalone feature**: the bridge opens its own connection to the
MeshCore device (BLE or USB serial, the `Connection` config section) and
needs nothing else — not the HTTP gateway, not the bot.

The XiaoZhi side uses the **MCP access point** protocol, and the direction of
the connection is the whole trick:

```
[XiaoZhi device] ⇄ ws ⇄ [xiaozhi.me cloud] ⇐= outbound wss =⇐ [meshcore-mcp-gateway]
                                                                  ⇅ BLE/serial
                                                            [MeshCore device]
```

The bridge **dials out** to xiaozhi.me and acts as an MCP (Model Context
Protocol) *server* on that connection — the cloud is the MCP client. Because
the connection is outbound, it works behind ISP NAT, CGNAT, firewalls: no port
forwarding, no public IP, no tunnel. This is the same mechanism XiaoZhi-based
integrations like nanobot use.

*[🇷🇺 Русская версия](README_RU.md)*

---

## What the assistant can do

| Tool | Voice example | What it does |
|---|---|---|
| `mesh_get_new_messages` | *"прочитай новые сообщения"* | Reads unread mesh messages (10 per call, pages through) |
| `mesh_send_message` | *"отправь Короеду: буду через час"* | Sends a dm or channel message over the radio |
| `mesh_search_contacts` | *"найди контакта Йорика"* | Searches contacts by a name fragment (case/emoji ignored) — how the assistant finds recipients in a large contact book |
| `mesh_list_contacts` | *"кто есть в сети?"* | Lists the first known node names — fine for small contact lists |
| `mesh_list_channels` | *"какие каналы есть?"* | Lists configured channels |

Tool failures come back as readable results ("recipient not found — use
mesh_list_contacts to check names") so the assistant can tell you what went wrong
instead of dying silently.

## Setup

**1. Get your MCP access point URL.** Log in to the [xiaozhi.me
console](https://xiaozhi.me) → open your agent (智能体) → find **MCP
设置 / 获取 MCP 接入点** and copy the endpoint URL
(`https://api.xiaozhi.me/mcp/?token=…`). It is per-agent: the tools appear on
that one agent.

**2. Configure and run:**

```yaml
# config.yaml (subset of the family format; unknown sections are ignored)
MCP:
  endpoint_url: "https://api.xiaozhi.me/mcp/?token=XXXX"  # from the console

Connection:
  type: "bt"             # "bt" (BLE, default) or "serial" — same as everywhere
  # serial_port: "/dev/ttyUSB0"
  # serial_baud: 115200

Shrink:                  # how voice messages are fitted to the mesh
  cyr_to_lat: true       # squeeze Cyrillic to identical-looking Latin (saves bytes)
  # max_bytes: 140       # hard mesh limit
  # overrides:
  #   xiaozhi: split     # voice already defaults to split; override if you like
```

```sh
go run .
```

The console's MCP status indicator should flip to connected within seconds.
Now just talk to your XiaoZhi device.

### Config reference

| Key | Default | Meaning |
|---|---|---|
| `MCP.endpoint_url` | *(required)* | The agent's MCP access point URL from the xiaozhi.me console |
| `Connection.*` | — | Which MeshCore device to attach to, and how |
| `Shrink.*` | voice defaults to `split` | Outgoing-message fitting — see the behavior notes below |

### One app per device

The bridge attaches to the companion device directly. Don't run it against
the **same physical device** as [meshbot](https://github.com/stempit/meshbot)
or the [HTTP gateway](https://github.com/stempit/meshcore-http-gateway) at
the same time — a MeshCore companion serves one BLE/serial connection.
Different devices are fine. (Need both the HTTP API and voice on one radio?
Run them at different times, or put a second radio on the desk.)

## Behavior details

- **Russian finds English names.** The assistant works in Russian, so contact
  matching is phonetic and language-agnostic: say *"Йорик"* and both search
  and send find `Yorick 💀` (and vice versa — a Latin-spelled query finds
  `Короед`). Matching first tries the exact stored name, then ignores case,
  emoji and punctuation, then falls back to a phonetic fuzzy match with a
  small edit budget. If several contacts match equally well, the assistant
  lists them and asks you to say the full name.
- **Sends are mesh-fitted.** Outgoing voice messages pass through the same
  `fit` package (from
  [meshcore-companion](https://github.com/stempit/meshcore-companion)) as
  every bot send. By default a message longer than the mesh limit is **split**
  into sequenced `[i/N]` parts, sent one after another (each takes up to ~30 s
  of radio time). With `Shrink.cyr_to_lat: true` Cyrillic is first squeezed to
  identical-looking Latin letters, buying extra bytes. Override the strategy
  with `Shrink.overrides.xiaozhi` (`truncate`, `split`, `skip`).
- **Voice cursor.** On startup the bridge first syncs messages stored on the
  device, then primes its cursor to the newest ID — so "new messages" means
  *arrived while the bridge is running*; the backlog that existed before
  startup is not read aloud. The cursor survives reconnects to xiaozhi.me;
  restart the bridge to start fresh.
- **Slow sends are normal.** Radio delivery confirmation takes up to ~30 s;
  `mesh_send_message` blocks that long and the assistant pauses before
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

`main.go` (composition root + config), `xiaozhi/session.go` (outbound
WebSocket + reconnect), `xiaozhi/protocol.go` (JSON-RPC/MCP wire handling +
tool implementations), `xiaozhi/mesh.go` (`Mesh` interface + direct device
backend), `xiaozhi/tools.go` (tool descriptors). The device runtime (connect,
startup sync, inbox, keepalive) comes from
[meshcore-companion](https://github.com/stempit/meshcore-companion); the
protocol driver from [meshcore-go](https://github.com/stempit/meshcore-go).

## License

MIT — see [LICENSE](LICENSE).
