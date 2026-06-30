# dnsmux

A tiny UDP DNS multiplexer. `dnsmux` listens on a single UDP socket (`:53` by
default), inspects the QNAME of each incoming query, and forwards the raw packet
to one of several backend DNS servers based on a suffix-to-backend routing table.
The backend's response is relayed back to the original client unchanged.

Queries whose QNAME matches no configured route are answered with `RCODE=REFUSED`.

## How it works

- Reads each UDP query and decodes the QNAME from the first question section.
  (Question-section labels are uncompressed per spec, so no compression-pointer
  handling is needed.)
- Matches the QNAME against the configured routes by suffix. **Longest suffix
  wins**, so a more specific zone overrides a less specific parent when both are
  configured.
- Forwards the original, unmodified query to the matching backend over UDP and
  relays the reply back to the client.
- Each query is handled in its own goroutine, with a per-backend response
  timeout.

## Build

```sh
go build -o dnsmux .
```

Requires Go 1.21+. No external dependencies.

## Usage

```sh
dnsmux -route SUFFIX=HOST:PORT [-route ...] [flags]
```

At least one `-route` is required.

### Flags

| Flag        | Default       | Description                                   |
|-------------|---------------|-----------------------------------------------|
| `-route`    | —             | Route as `suffix=host:port`; may be repeated. |
| `-listen`   | `0.0.0.0:53`  | UDP address to listen on.                     |
| `-timeout`  | `2s`          | Backend response timeout.                     |
| `-v`        | `false`       | Log every query and routing decision.         |

### Example

Route internal `corp.example.com` queries to one resolver and a test zone
`t.example.com` to another:

```sh
sudo ./dnsmux \
  -route corp.example.com=10.0.0.53:53 \
  -route t.example.com=127.0.0.1:5301 \
  -v
```

A query for `app.t.example.com` is forwarded to `127.0.0.1:5301`, while
`host.corp.example.com` goes to `10.0.0.53:53`. Anything else (e.g.
`google.com`) is answered `REFUSED`.

Binding to port 53 typically requires elevated privileges (`sudo`, or a
`CAP_NET_BIND_SERVICE` capability).

## Notes & limitations

- **UDP only.** TCP DNS (and EDNS0 fallback to TCP) is not handled.
- Backends are contacted over UDP; responses larger than 4096 bytes are
  truncated by the read buffer.
- No caching, no retries — `dnsmux` is a stateless forwarder.
- Trailing dots and case are normalized when matching suffixes.

## License

See repository for license details.
