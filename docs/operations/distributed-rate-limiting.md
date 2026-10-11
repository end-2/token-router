# Distributed memory rate limiting

With `rate_limit.backend: memory` and `rate_limit.memory.distributed.enabled: true`, multiple token-router processes share request count and token usage balances without Redis. Each process participates as a Raft voter, and the elected leader serializes admission decisions and usage recording. Results return after applying commands recorded on a majority. With account limiting enabled, balances are shared for the same account; otherwise, the entire cluster shares one bucket.

## Recovery and availability

For production, use an odd number of voters, at least three. A majority must communicate to complete admission decisions, such as two of three or three of five. A single node is allowed but has no fault tolerance. A two-node cluster loses its majority when either node stops.

When the leader fails, the remaining majority elects another leader and retains replicated balances. During elections or when commands cannot be recorded on a majority, admission fails with `503`. There is no automatic fallback to per-process `memory` buckets. `/readyz` reports only HTTP server startup and shutdown state and does not guarantee cluster majority availability.

Relative `data_dir` paths are resolved from the main configuration file's directory. Use a separate directory for each node and retain the same address and directory across restarts. Containers need persistent volumes writable by their runtime UID. Restarts recover balances from snapshots and logs. Logs and snapshots use SHA-256 account identifier hashes and do not store raw authentication headers or tokens. Deleting every node's data makes existing usage unrecoverable.

All nodes use timestamps recorded in commands by the leader. If the clock moves backward after leader replacement, buckets do not refill using a time earlier than their last recorded timestamp. Large forward clock jumps can refill accordingly, so synchronize node clocks.

A command may already be recorded if a connection closes or a timeout occurs before the response arrives. Internal mutations are not automatically retried. Requests ending with `503` may still consume request units, and failed usage recording can leave some usage uncounted. Upstream responses already started continue unchanged, and usage recording failures emit warnings.

Every admission decision and usage record waits for disk writes on the leader and a majority. Network latency and storage performance affect throughput. Idle account state can be cleaned up after enough time to refill all buckets completely; state with remaining token debt is retained.

## Configuration and service discovery

The following `config.yaml` configures the first node. `peers` lists every node's `IP:admin-port`, including the node itself. All nodes use the same list and shared token, while `address` and the data directory are specific to each node.

```yaml
server:
  address: "0.0.0.0:8080"
admin:
  enabled: true
  address: "0.0.0.0:9091"
policy:
  file: policy.yaml
rate_limit:
  enabled: true
  backend: memory
  memory:
    distributed:
      enabled: true
      address: "10.0.0.11:9091"
      peers:
        - "10.0.0.11:9091"
        - "10.0.0.12:9091"
        - "10.0.0.13:9091"
      data_dir: data/rate-limit
      request_timeout: 5s
```

Inject the shared token through `TOKEN_ROUTER_RATE_LIMIT_MEMORY_DISTRIBUTED_TOKEN`; the example stores no actual token. Set the second and third nodes' `address` to their own IPs. Multiple nodes can run on the same host with separate admin ports and data directories. API and Prometheus listener ports must also avoid conflicts.

| Setting | Meaning and default |
| --- | --- |
| `rate_limit.memory.distributed.enabled` | Defaults to `false`; enables distributed mode when `memory` is selected |
| `rate_limit.memory.distributed.address` | This node's `IP:admin-port` reachable by other nodes; required when enabled |
| `rate_limit.memory.distributed.peers` | All voter addresses, including this node; use the same membership on every node |
| `rate_limit.memory.distributed.data_dir` | Storage for Raft logs, voting state, and snapshots; defaults to `data/rate-limit` |
| `rate_limit.memory.distributed.token` | Shared secret for internal request authentication; required when enabled |
| `rate_limit.memory.distributed.request_timeout` | Maximum wait for each admission decision or usage record; positive, defaults to `5s` |

`address` and `peers` allow only canonical IP addresses with ports from 1 to 65535. IPv6 uses the form `[2001:db8::11]:9091`. Hostnames, URLs, duplicate peers, and wildcard addresses are rejected. `admin.address` is the actual bind address, so it may use `0.0.0.0:9091` or `[::]:9091`, but its port must match the advertised `address`.

Service discovery uses a static peer list. Automatic joining through seed lists, DNS lookup, and runtime membership changes are unsupported. `peers` may differ in order but must contain the same members. Addresses and membership are checked against identity metadata in the stored data directory, so startup rejects existing data used with a different address or voter list.

Each setting is also available through CLI arguments and environment variables derived from its path.

```sh
go run ./src/cmd/token-router serve --config config.yaml \
  --rate-limit-memory-distributed-enabled \
  --rate-limit-memory-distributed-address=10.0.0.11:9091 \
  --rate-limit-memory-distributed-peers='["10.0.0.11:9091","10.0.0.12:9091","10.0.0.13:9091"]'
```

## Policies and accounts

Place limit values and account extraction settings in `policy.yaml`. The following configuration gives every account a request burst of 20 and a token burst of 60,000.

```yaml
rate_limit:
  requests_per_second: 10
  burst: 20
  tokens_per_second: 1000
  token_burst: 60000
  account:
    enabled: true
    jwt_claim: sub
```

See [Per-account limits](../reference/configuration.md#per-account-limits) for the distinction between account extraction and authentication. With `account.enabled: false`, these values apply to the entire cluster.

Deploy the same policy to all nodes. Policy files and admin `/config` changes apply only to the receiving node; Raft does not distribute policy files. If follower-supplied limit values or account extraction settings for new requests differ from the leader's, requests return `503`. This can occur temporarily during rolling policy deployment. Usage recording retains the account determined at admission.

When applying a command with new values, distributed buckets compute the account balance using the previous refill rate, then apply the new capacity. Increasing capacity does not refill existing balances, and token debt is preserved. Disabling and reenabling token limits follows the usage observation rules in [Policy configuration](policy-configuration.md#file-watching-and-application).

## Admin port and internal communication

Admin communication uses HTTP without TLS. Restrict access to a trusted management network allowing direct peer connections. Internal route token checks do not protect `/config`, which has no separate authentication. Restrict external port access and policy modification permissions at the network layer.

No additional Raft port is needed. The admin listener registers these internal routes:

| Method and path | Behavior |
| --- | --- |
| `CONNECT /internal/rate-limit/raft` | After authentication, hands off the Raft connection for voting, log replication, and snapshot transfer |
| `POST /internal/rate-limit/apply` | Followers forward admission decisions or usage records to the leader |

Internal requests verify the shared token and a cluster identifier calculated from the peer list. A different peer list or invalid token is rejected. Internal routes are not exposed on API or Prometheus listeners. Proxy environment variables and HTTP redirects are not used.

## Implementation references

[`distributed.go`](../../src/internal/bucket/distributed/distributed.go) uses `BootstrapCluster`, `Apply`, and `Shutdown` from `hashicorp/raft` v1.8.0 and `New` from `raft-boltdb/v2` v2.4.2. The `Apply` timeout bounds only command enqueueing, so a separate context bounds the caller's total wait. See the official implementations in [Raft v1.8.0 api.go](https://github.com/hashicorp/raft/blob/v1.8.0/api.go) and [raft-boltdb v2.4.2 bolt_store.go](https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/bolt_store.go).

[`distributed/transport.go`](../../src/internal/bucket/distributed/transport.go) implements [Raft v1.8.0 net_transport.go's StreamLayer](https://github.com/hashicorp/raft/blob/v1.8.0/net_transport.go) over admin HTTP CONNECT. [`distributed/state.go`](../../src/internal/bucket/distributed/state.go) manages replicated timestamps, per-account balances, and snapshots.
