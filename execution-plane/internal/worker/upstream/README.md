# Bounded HTTP response relay

`Relay` and `ReadUnary` consume an already received HTTP response. They never
create clients, choose proxies, access credentials or retry a request. The
worker adapter owns request creation, authentication and protobuf mapping.

- `IsStreaming` is the shared classifier: HTTP 2xx and a valid
  `text/event-stream` media type. A malformed nonempty Content-Type is rejected
  before reading/delivery; an absent media type follows the bounded unary path.
- SSE reads are at most 32 KiB and synchronous with delivery, without a
  cumulative body slice. RPC deadlines remain the overall execution bound.
- Unary and non-2xx bodies retain the independent 2 MiB cap and are validated
  before any headers/body are delivered. This is a local limit, not evidence
  of parity with every production request/response size.
- `Sink.Chunk` and the observer borrow bytes only for their synchronous call.
  The worker RPC adapter copies each chunk before handing it to gRPC.
- Context cancellation closes the response body once, including when a read
  is blocked. Sinks must obey their context; the module does not spawn an
  unbounded goroutine to abandon an arbitrary noncooperative sink.
- Remaining non-identity content encoding is rejected. Normal Go Transport
  gzip decoding works before this boundary. Only content-type/x-request-id
  response headers cross the existing RPC contract.
- Relay success is HTTP EOF only. The adapter requires the separate bounded
  `upstreamusage.SSE` observer to accept message completion. Read/delivery/
  semantic failures do not produce a successful Completed response. Non-2xx
  responses keep status/body and no usage; their Completed means response-end,
  not model success.

The usage observer recognizes fixed nonnegative integer fields and cumulative
counts, not a new billing authority. It bounds JSON to 2 MiB and SSE lines/events
to 1 MiB. It handles future well-formed event types inside an active message
without treating them as usage or completion, consistent with the
[Anthropic streaming contract](https://platform.claude.com/docs/en/build-with-claude/streaming).
No partial-failure accounting is added: the current RPC has usage only on
Completed, and a truncated/error stream must not be labeled successful to
transport its partial counters. That remains a later gateway/protocol gate.

Local verification (synthetic HTTP and RPC only):

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test -race -count=1 ./internal/worker/...
```

This does not implement cli_native, a production host-agent, a new gateway
dispatch path, or full Docker/online compatibility.
