# Two-node mesh

Two `dcrdex` processes run one DEX. The **master** runs markets and swaps
and writes the event log. The **slave** forwards client work to the master
and copies the log so it can take over. Clients may connect to either node.

Omit every mesh option to run a single server. Do not configure a mesh peer
against a server running in single-server mode.

> **Two-node limitation:** a network partition can leave both nodes serving
> independently. See [Stop, restart, and failover](#stop-restart-and-failover).

## What must be the same

A mismatch in any compatibility setting rejects the handshake.

- Use the same `dcrdex` release on both nodes. The API, event schema, and mesh
  protocol versions must match.
- Use the same DEX signing private key. This is normally `sigkey` in appdata,
  or the file named by `--dexprivkeypath`. If you copy an encrypted key file,
  each node must be able to unlock it.
- Use the same network (`--testnet` or `--simnet`; mainnet is the default).
- Use the same `--bcasttimeout`, `--txwaitexpiration`, `--cancelthresh`,
  `--freecancels`, `--maxepochcancels`, and `--penaltythreshold` values.
- Use the same market definitions and asset settings:
  assets, fee and confirmation limits, registration and bond settings, market
  pairs, lot and parcel sizes, rate steps, epoch durations, and buy buffers.

Copying `markets.json` is the simplest approach when its paths work on both
hosts. Host-specific backend settings such as `configPath` and `nodeRelayID`
may differ; they are not part of the mesh compatibility check.

## What must be different

Each node needs its own:

- PostgreSQL database (both databases may be on the same PostgreSQL server);
- appdata directory;
- client listener (`rpclisten`);
- mesh listener (`meshlisten`);
- advertised client address (`clientaddr`).

`meshpeer` points to the other node's `meshlisten` address.

If both processes run on one host, `rpclisten` and `meshlisten` must use
different ports.

Each node creates `appdata/data/<network>/mesh/nodeid`. Do not copy this file
between nodes or change it after first start.

## Mesh settings

Add these mesh settings to each node's otherwise complete `dcrdex.conf`.

```ini
# On each node
rpclisten=0.0.0.0:7232
meshlisten=0.0.0.0:7233
meshpeer=wss://peer.example.com:7233
meshpeercert=/secure/peer.rpc.cert
clientaddr=this-node.example.com:7232
```

Client RPC and the mesh listener share the same TLS setting; they cannot be
configured independently. Without `--notls`, both use TLS and `meshpeer` uses
`wss://`. Each node uses its own `rpc.cert` and `rpc.key` for both client and
incoming mesh connections. Keep `rpc.key` private and do not share it between
nodes. Copy only the public certificates. On each node, `meshpeercert` points
to the other node's `rpc.cert`.

Each certificate must include the hostnames or IPs through which clients and
the peer reach that node. When `dcrdex` generates `rpc.cert`, set
`altdnsnames` in `dcrdex.conf` (or pass `--altdnsnames`) before the certificate
is created. This setting does not modify an existing certificate. If you
provide your own certificate, include the required names when issuing it.

With `--notls`, both client RPC and the mesh listener are plaintext,
`meshpeer` uses `ws://`, and `meshpeercert` is omitted.

`meshlisten` is used only for communication between the two nodes. Clients
connect to `rpclisten`. Firewall rules may restrict `meshlisten` to the other
node's IP address.

To inspect mesh status, enable the admin server on both nodes with
`--adminsrvon`. Keep `--adminsrvaddr` on loopback; the default is
`127.0.0.1:6542`.

`--noresumeswaps` cannot be used in mesh mode.

## Start

Sync every asset backend on both nodes before starting `dcrdex`. A master
cannot finish startup if a required backend is not ready.

Start both nodes together. A node with an empty event log must connect to its
peer, receive its initial snapshot, and load it within ten minutes. If it does
not, it exits and can be restarted. The deadline prevents an uninitialized node
from waiting indefinitely. A node with existing history waits indefinitely for
its peer.

Role selection works as follows:

- If one event log is an exact prefix of the other, the node with the longer
  log becomes master.
- If the logs are equal and neither node is already master, the node with the
  lower node ID becomes master.
- If the logs are equal and only one node is already master, it remains master.
- A fresh node copies the current state from the master, then follows new
  events.

If both nodes report master or their histories conflict, see [Fork](#fork).

Client and admin ports do not open until the handshake, any snapshot seed,
state loading, and master preparation have completed. Watch the `dcrdex` logs
during initial startup. After startup, query `GET /api/mesh` on each node's
admin server.

A healthy pair has:

- one `established_master` and one `established_slave`;
- `ready: true`, `stateLoaded: true`, and `peerConnected: true` on both;
- neither node reports `seeding: true`;
- after replication catches up, both nodes report the same `frontierSeq` and
  `frontierHash`, and the master reports no positive `streamLag`.

| `mode` | Meaning |
| --- | --- |
| `pending` | Waiting for a handshake |
| `preparing_master` | Loading state and starting master workers |
| `established_master` | Running markets and writing the event log |
| `established_slave_syncing` | Connected and catching up |
| `established_slave` | Caught up and forwarding client work |
| `slave_no_master` | Master connection lost; promotes at `promoteAt` |
| `halted` | Terminal; the process will exit. Read `haltErr` in logs or status |

Read-only admin requests use the local replica. Mesh-backed changes such as
market suspend/resume, prepaid bond creation, and user forgiveness may be sent
to either established node; the slave forwards them. Node-local settings such
as fee scaling and data API enablement must be changed on each node.
`forgive_match` is not supported in mesh mode; use `forgive_user`.

## Upgrade a legacy single server

Keep the existing server as Node A and add a fresh Node B. Do not copy the
existing database to Node B.

1. Prepare Node B with synchronized asset backends, a fresh appdata directory,
   a fresh PostgreSQL database, and its own RPC certificate. Copy the DEX
   signing key and the settings listed under
   [What must be the same](#what-must-be-the-same) from Node A.
2. Stop the legacy server. Back up its PostgreSQL database, appdata, and signing
   key.
3. Install the same mesh-capable `dcrdex` release on both nodes. Add the mesh
   settings to Node A while keeping its existing database and appdata.
4. Start Node A, then start Node B immediately. Do not wait for Node A's client
   port before starting Node B; client service waits for the mesh to connect.
5. Watch the logs. Node A's database is upgraded automatically, Node A becomes
   master, and Node B copies its state.
6. After both client ports open, verify that one node is
   `established_master` and the other is `established_slave`.

The database upgrade cannot be opened by the older server software. To roll
back, stop both nodes and restore Node A's backup. Never upgrade two separate
copies of the legacy database; they will be treated as different histories.

Update clients to a mesh-aware release before relying on automatic failover.

## Stop, restart, and failover

Stopping the slave removes redundancy but does not interrupt trading. The
master keeps serving.

On SIGINT or SIGTERM, the master stops producing work and waits up to 30
seconds for the slave to apply every committed event. If the drain succeeds,
the master requests a planned handoff and the slave begins promotion
immediately. Wait until the other node reports `established_master` before
restarting the stopped node.

A crash cannot drain or request a handoff. The slave enters `slave_no_master`
and promotes after 30 seconds if the master remains unreachable. A failed
drain follows the same delayed path.

During a network partition, the old master does not step down. After 30
seconds, the slave promotes and may also accept work. When the mesh link
returns, the nodes compare their histories and one or both may shut down. If
both accepted different work, both shut down. See [Fork](#fork) for recovery.

## Fork

The mesh cannot merge two different histories. Any trades or other changes
recorded only in the database you replace will be lost.

If one node remains `established_master`:

1. Leave the master running.
2. Back up the halted node's database.
3. Replace the halted node's PostgreSQL database with an empty database. Keep
   that node's appdata, configuration, signing key, and RPC certificate.
4. Start the rebuilt node. It copies the current state from the master.

If both nodes halt:

1. Back up both databases.
2. Compare the logs and choose which node's history to keep.
3. Leave that node's database and appdata unchanged.
4. Replace the other node's PostgreSQL database with an empty database. Keep
   its appdata, configuration, signing key, and RPC certificate.
5. Start the node whose history you kept, then start the rebuilt node
   immediately. The rebuilt node joins by snapshot.

If a halted joining node's log contains a reset token, you may reuse its
database instead of replacing it:

```text
MESH FORK DETECTED: … --meshforkreset=<seq>:<tiphash-prefix>
```

`--meshforkreset` is an optional convenience. The token confirms that the
database still has the event-log tip shown in the halt message before it is
wiped.

1. Back up the halted node's database.
2. Restart that node once with the token exactly as logged. It wipes the local
   event history and trading data, then copies the current state from the
   master.
3. Remove `--meshforkreset` before the next start.
