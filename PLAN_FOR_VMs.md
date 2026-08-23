# QEMU/KVM guest monitoring plan

Status: proposal, based on the repository and upstream documentation available on 2026-08-22.

## Decision

In this project, "monitor a VM" currently means collecting the guest's structured
systemd journal and exporting it as OTLP logs. It does not merely mean detecting
whether QEMU is running or reporting host-side CPU and memory counters.

There is no transparent host-side equivalent of `pct exec ... journalctl
--follow` for a QEMU guest. The recommended approach is therefore two-track:

1. For production fleets where reliability, scale, multiple operating systems,
   and application telemetry matter, run a collector or log forwarder in each
   guest. Prefer OTLP directly; use `systemd-journal-upload` and a central
   `systemd-journal-remote` receiver when preserving the native journal is more
   important than avoiding a journal relay. These are deployment solutions, not
   features that `pve2otelcol` should try to hide.
2. For Linux/systemd guests where installing a dedicated telemetry agent is not
   acceptable but QEMU Guest Agent (QGA) is already enabled, prototype an
   **optional cursor-based finite-batch monitor** in `pve2otelcol`. Do not try to
   stream `journalctl --follow` through QGA. Keep this mode off by default until
   the validation gates below pass on the supported Proxmox versions and at the
   intended fleet size.

Do not adopt SSH, an interactive serial console, live disk inspection, or a
custom virtio-serial protocol as the general QEMU implementation. They either
move credentials and guest-network management into this service, lose journal
replay semantics, are unsafe against live disks, or amount to maintaining a new
guest agent and protocol.

Host-visible VM lifecycle and resource metrics should be a separate feature.
Proxmox already exposes those facts without entering the guest, but they cannot
replace guest logs, guest filesystem/process metrics, or application telemetry.

## Scope and terminology

This plan distinguishes three observability layers:

| Layer | Examples | Best source |
| --- | --- | --- |
| Hypervisor/VM lifecycle | running, stopped, migration, allocated vCPU/RAM, host-observed CPU, disk and network I/O | Proxmox API/RRD/QMP |
| Guest OS | journal, Windows Event Log, filesystem usage, processes, guest load | software or an agent API inside the guest |
| Application | OTLP logs, metrics, traces and application resource attributes | application SDK or guest-local collector |

The proposed QGA mode covers only structured journal logs from Linux guests that
have systemd, `journalctl`, a working QGA channel, and permission for
`guest-exec`. It does not make Windows, BSD, appliances, or QGA-disabled guests
monitorable. A guest-local collector is the appropriate cross-platform answer.

## Current repository state

- `pvesh get /cluster/resources --type vm` already discovers both `lxc` and
  `qemu` resources and filters them to the local node and running state.
- [`pve/pve.go`](pve/pve.go) contains a dormant `CurrentKVMs` implementation,
  but `CurrentVMs` does not call it. The stub uses `qm exec`; the current
  Proxmox command is `qm guest exec`.
- More importantly, changing the command name would not fix the design. QGA
  returns captured stdout/stderr only after the guest process exits. A
  successful `journalctl --follow` is deliberately endless, so no records
  become available to the current streaming parser.
- QEMU currently caps each captured `guest-exec` stdout and stderr stream at
  16 MiB and reports whether either was truncated. Any batch design must treat
  truncation as a normal, tested condition rather than assume output is
  complete.
- The existing type-qualified source IDs, per-source journal cursors, record
  conversion, OTLP logger construction, reconciliation, shutdown, and
  Prometheus health endpoint are reusable.
- The current monitor abstraction assumes one long-running subprocess with a
  readable stdout stream. A QGA poller is a different monitor kind and should
  not be disguised as another `MonitorCmd`/`MonitorArgs` pair.
- A QEMU source should be identified as `qemu/<vmid>` (and persisted as
  `qemu-<vmid>.cursor`), not `qm/<vmid>`. `qm` is a CLI name, while `qemu` is
  the Proxmox resource type already used in discovery.

Relevant constraints are documented by the [QGA protocol][qga-protocol], the
[QEMU capture implementation][qga-source], and the [Proxmox `qm` command
reference][proxmox-qm].

## Options evaluated

| Option | Guest requirements | Delivery and fidelity | Operational/security cost | Conclusion |
| --- | --- | --- | --- | --- |
| Endless `qm guest exec ... journalctl --follow` | QGA, Linux, systemd, unrestricted guest exec | No live output; QGA exposes captured output only after exit | Leaves a long-running privileged guest process and cannot feed the current parser | Reject |
| Finite QGA cursor batches | QGA, Linux, systemd, unrestricted guest exec | Structured JSON and host-owned cursor replay are possible; polling latency, 16 MiB capture limit, and shutdown tail loss remain | No guest network or new daemon, but repeated privileged commands and QGA load | Prototype as opt-in |
| QGA-managed spool file plus `file-read` | QGA plus either unrestricted exec or a preinstalled guest service | Can decouple command completion from reads, but rotation, acknowledgement, truncation and cleanup become a new queue protocol | Writes guest state, needs more privileges, and [newer Proxmox versions improve offset reads][proxmox-file-read] but do not solve queue semantics | Fallback experiment only if finite batches fail on throughput |
| Guest-local OpenTelemetry Collector/Alloy/other agent | A managed package/service and outbound OTLP | Native streaming, local checkpointing/buffering options, host metrics, application signals and Windows support | Per-guest lifecycle, configuration, credentials and upgrades | Recommended production path |
| `systemd-journal-upload` to `systemd-journal-remote` | systemd remote-journal package, guest network and preferably mutual TLS | Preserves native structured journal and can create a receiver-side durable journal split by host | Operates a TLS listener and central disk retention; VMID-to-host identity must be made explicit | Strong Linux-only alternative |
| Syslog forwarding | Guest syslog configuration and network | Mature and widely supported, but may lose journal-only fields; delivery depends on UDP/TCP/TLS and queue settings | Additional receiver and identity mapping | Accept only when reduced fidelity is acceptable |
| SSH from the PVE node | Reachable guest IP, sshd, account/key, host-key management and journal permission | True stream and cursor replay are possible | Makes this root service a fleet SSH credential and network-discovery manager; cloning and host-key changes are difficult | Do not build in |
| Serial console or custom virtio-serial channel | VM hardware change and guest producer/service | Raw streaming is possible, but a console has no record acknowledgement or replay; a reliable custom channel needs a protocol and guest daemon | Conflicts with console use and increases migration/configuration surface | Reject as general solution |
| Read/mount guest disks from the host | Storage-format and filesystem support; often elevated storage access | A live filesystem is not a coherent journal transport; encrypted, remote and passthrough storage break the model | High corruption, consistency and credential risk | Reject; offline forensic use only |
| Proxmox API/RRD/QMP only | None beyond existing PVE access | Good for lifecycle and host-observed resource metrics; cannot read the guest journal | Low incremental cost | Implement separately if VM health metrics are desired |

The OpenTelemetry [agent deployment pattern][otel-agent] describes the
guest-local approach. The contrib [journald receiver][otel-journald] already
supports cursor storage and structured journal parsing, but its logs component
is currently alpha and requires a compatible `journalctl` plus journal read
permission. That maturity level must be considered when choosing the actual
guest agent. systemd documents that `systemd-journal-upload` sends existing and
new journal entries over HTTP(S), while `systemd-journal-remote` stores received
records as journal files; see the [upload][journal-upload] and
[remote][journal-remote] documentation.

## Proposed QGA finite-batch design

This section is an implementation candidate, not a claim that QGA has become a
streaming transport.

### Record flow

1. Discover local running `qemu` resources from the same successful Proxmox
   snapshot used for LXC discovery. Do not run a second cluster-resource query.
2. Probe each QEMU source independently. Confirm that QGA responds, that
   `guest-exec` is enabled, and that an absolute `journalctl` path exists.
   Cache only successful capabilities against a guest identity. A failed QEMU
   probe must not fail the entire discovery snapshot or stop working LXC/PVE
   monitors.
3. Load the host-side cursor for `qemu/<vmid>`. If none exists, establish the
   current journal tail with a finite `journalctl --lines=0 --show-cursor`
   command and export no historical records. Historical backfill should require
   an explicit option.
4. Poll with an argument array equivalent to:

   ```text
   /absolute/path/to/journalctl
     --after-cursor=<cursor>
     --lines=+<batch-size>
     --output=json
     --no-pager
   ```

   The `+` form selects the oldest matching records, so repeated batches drain
   a backlog in order. Pass every value as a distinct QGA argument; do not
   construct a shell command or interpolate the cursor into shell syntax.
5. Start the finite command through the Proxmox guest-agent API, poll
   `guest-exec-status` with a bounded deadline, and inspect exit status,
   base64 output and truncation flags. QGA output is not consumable before exit.
6. Decode complete newline-delimited JSON records through the existing journal
   conversion path. Advance and persist the host-side cursor only after a
   complete record is handed to the in-process OpenTelemetry logger, retaining
   the project's current best-effort delivery contract.
7. If a batch is full, poll again immediately to drain backlog. Sleep for the
   configured polling interval only after an under-full batch.
8. On cancellation, stop scheduling calls and make a bounded best-effort attempt
   to terminate a known guest PID. QGA has no native `guest-exec-cancel` command;
   a host-side timeout stops waiting but does not prove that the guest process
   died. The worker must report an orphan-risk diagnostic rather than claiming
   successful cancellation.

### Truncation and oversized records

The QEMU implementation limits captured stdout and stderr independently to
16 MiB. The poller must never advance to an unseen cursor merely because QGA set
`out-truncated`.

- Parse and commit only complete JSON records ending before the truncated tail.
- Retry from the last committed cursor with a smaller batch, down to one record.
- If a single record still cannot fit or exceeds this project's 1 MiB record
  limit, obtain that record's cursor with a cursor-only query, increment an
  explicit oversized/drop metric, log the gap with `source` and `cursor`, and
  advance past exactly that record. This prevents an infinite retry loop while
  keeping data loss visible.
- Treat malformed QGA envelopes, invalid base64, a missing truncation field when
  the payload reaches the known ceiling, and nonzero guest exit status as batch
  failures. Do not change the cursor.

### Cursor invalidation and VM identity

A cursor can disappear after journal vacuuming, guest restoration, VMID reuse,
or replacement of the guest OS. The behavior must be configurable and visible:

- default: log a warning, increment `qemu_cursor_resets_total`, reset to the
  current tail, and state that a gap occurred;
- strict mode: leave the source unhealthy and retain the cursor;
- optional recovery mode: restart from the oldest retained entry, accepting a
  potentially large replay.

`qemu/<vmid>` remains the stable OpenTelemetry service instance identity across
migration and rename. Capability caches should also use a guest-incarnation
fingerprint when Proxmox exposes a stable VM generation/SMBIOS UUID; node,
VMID, and name alone do not detect replacement under the same name. Do not put
the guest's journal cursor, credentials, or raw QGA responses in normal logs.

### Configuration

Introduce an explicit mode instead of reviving the commented `SkipKVMs` field:

```text
--qemu-monitoring=off|qga-poll
--qemu-poll-interval=<duration>
--qemu-batch-size=<positive integer>
--qemu-command-timeout=<duration>
--qemu-cursor-reset=tail|oldest|fail
```

Initial defaults should be `off`, a polling interval chosen by the load test,
and conservative batch and timeout values. Include/exclude VMID filters should
continue to apply to both LXC and QEMU resources. Dry-run output must state the
monitor kind and capability command without executing anything in the guest.

Do not automatically install QGA, enable the VM's QGA device, change QGA's RPC
allow/block list, modify VM hardware, or write guest credentials. Those are
deployment actions outside this program's authority.

### Internal structure

- Replace the implicit "every source is one streaming command" assumption with
  two explicit monitor strategies: `stream` for PVE/LXC and `qga-poll` for QEMU.
- Keep discovery metadata separate from runtime state. A monitor strategy should
  own its command/status calls but reuse the existing record handler, cursor
  store, logger and managed worker lifecycle.
- Add a small injected QGA interface for `Probe`, `Start`, `Status` and
  best-effort termination. Its production implementation can use the local
  Proxmox API/CLI; tests must use deterministic fakes and recorded JSON
  envelopes.
- Preserve structured diagnostics (`source`, `command`, `error`, `retry`) and
  add QGA-specific context such as `pid`, `batch_size` and `truncated` without
  logging record bodies.
- Reconcile QEMU rename and migration with the existing logger replacement and
  source removal behavior. Since batches are finite, removal should wait only
  for the bounded in-flight operation, not for a persistent guest command.

### Service metrics

At minimum expose aggregate, low-cardinality evidence for:

- eligible and skipped QEMU sources;
- QGA probe and poll failures;
- completed batches and records;
- truncated batches;
- cursor resets/gaps;
- guest-command timeout/orphan risk; and
- the existing oversized and dropped record counters.

Per-VM details belong in structured service logs keyed by `source`; do not add a
Prometheus label containing every VMID unless the cardinality policy is decided
explicitly.

## Validation gates

### Unit and race tests

All tests remain independent of a live Proxmox host and collector.

- QEMU and LXC are built from one discovery snapshot, with local-node, status,
  include and exclude matrices.
- A missing/broken QGA or missing `journalctl` skips only that QEMU source.
- Argument encoding never invokes a shell and preserves an opaque cursor.
- Initial tail selection exports no old record; explicit oldest mode does.
- Multiple finite batches preserve order and immediately drain backlog.
- Empty batches sleep; cancellation interrupts the sleep promptly.
- Exit failures, API timeouts, malformed JSON/base64 and QGA restarts retain the
  last committed cursor.
- Truncated output commits only its complete prefix, shrinks the retry batch and
  cannot loop forever on one oversized record.
- Invalid/vacuumed cursors exercise every reset policy and emit metrics.
- Rename, stop, removal, migration and shutdown do not leak logger providers or
  workers.
- `go test -race ./...` covers simultaneous discovery, polling, cursor updates
  and shutdown.

### Disposable PVE/guest spike

Before implementation is enabled in a release, capture the exact JSON produced
by every supported Proxmox version for asynchronous guest exec and exec status.
The spike should use a disposable Linux VM and begin with these operator checks:

```sh
VMID=101
qm config "$VMID" --current
qm guest cmd "$VMID" ping
qm guest exec "$VMID" -- /bin/sh -c 'command -v journalctl'
qm guest exec "$VMID" -- /usr/bin/journalctl --lines=2 --output=json --no-pager
```

Then prove, with generated unique journal messages and cursor assertions:

1. no-backfill startup followed by a new record;
2. service restart and replay after the last persisted cursor;
3. a backlog larger than one batch and a payload larger than QGA's 16 MiB
   capture limit;
4. one malformed and one over-1-MiB journal record;
5. QGA disabled, QGA RPC blocked, QGA restarted and `journalctl` absent;
6. guest reboot, cursor vacuum, VM shutdown during a poll, VM rename and live
   migration to/from the local node;
7. collector outage under the existing best-effort OTLP contract; and
8. graceful `pve2otelcol` shutdown while a status request is in flight.

Record CPU time, guest-agent calls per second, p95 log latency and final-tail
loss at 1, 10, 50 and the expected maximum number of VMs. Select poll and batch
defaults only from those measurements. If QGA traffic materially delays backup,
freeze/thaw, shutdown, or other Proxmox agent operations, do not ship the mode.

### Acceptance criteria

The experimental implementation is acceptable only if it demonstrates:

- no silent cursor advancement on error or truncation;
- bounded memory, output size, subprocess count and shutdown time;
- documented p95 delivery latency and practical fleet-size ceiling;
- deterministic behavior for QGA-disabled and non-systemd guests;
- no regression to PVE/LXC collection when a QEMU source fails;
- clear, tested tail-loss behavior when a VM stops before its next poll; and
- an explicit security warning that QGA `guest-exec` permits privileged command
  execution in the guest and cannot be restricted to only the desired
  `journalctl` arguments by `pve2otelcol`.

## Rollout plan

1. **Spike:** validate Proxmox/QGA envelopes, output limits, cursor batching,
   cancellation behavior and load. Keep all product defaults unchanged.
2. **Refactor:** introduce monitor strategies and shared one-snapshot discovery
   without changing existing PVE/LXC behavior.
3. **Experimental implementation:** add QGA capability detection, finite-batch
   polling, truncation recovery, metrics and deterministic tests behind
   `--qemu-monitoring=qga-poll`.
4. **Operational documentation:** update `README.md`, `--help`, the systemd unit
   notes and troubleshooting with the Linux/systemd/QGA requirements, root/QGA
   privilege implications, latency, known tail-loss window, minimum Proxmox/QEMU
   versions, and rollback (`--qemu-monitoring=off`). Correct the current
   `qm exec` explanation even if the experimental feature is not enabled.
5. **Canary:** run on a small noncritical fleet through reboot, backup,
   migration, collector outage and version upgrade. Compare emitted unique test
   IDs with the guest journal to quantify loss and duplication.
6. **Decision:** retain QGA polling as opt-in unless measured scale and delivery
   behavior justify an `auto` mode. Do not make unsupported guests a startup
   error. If the gates fail, close the built-in QEMU effort and document the
   guest-local OTLP and journal-upload patterns as the supported solutions.

## Documentation implications

The current README says no agent is needed on individual VMs. That remains true
for LXC namespaces, but a QGA implementation requires an agent inside every
eligible QEMU guest. The accurate claim would be: no **pve2otelcol-specific**
guest agent is required; optional QEMU collection reuses QGA and requires
privileged `guest-exec`.

Document separately:

- what `pve2otelcol` observes from the hypervisor;
- what QGA polling can collect from Linux/systemd guests;
- why guest-local telemetry is preferred for metrics, traces, applications and
  Windows;
- how to disable QEMU polling instantly without affecting PVE/LXC monitoring;
  and
- how to verify QGA and journal access without exposing credentials or journal
  contents in bug reports.

## References

- [Proxmox `qm` CLI reference: guest exec, asynchronous PID/status, timeout and
  serial terminal][proxmox-qm]
- [QEMU Guest Agent protocol: output is returned after exit and may be
  truncated][qga-protocol]
- [QEMU source: 16 MiB captured-output and 48 MiB file-read limits][qga-source]
- [QEMU Guest Agent transport and RPC allow/block lists][qga-overview]
- [Proxmox QGA file-read implementation and offset/count support][proxmox-file-read]
- [QEMU character backends and their raw stream/file/socket semantics][qemu-chardev]
- [systemd `journalctl`: follow, JSON, cursor and oldest-line options][journalctl]
- [systemd journal upload documentation][journal-upload]
- [systemd journal remote documentation][journal-remote]
- [OpenTelemetry Collector agent deployment pattern][otel-agent]
- [OpenTelemetry contrib journald receiver requirements, cursor storage and
  alpha status][otel-journald]

[proxmox-qm]: https://github.com/proxmox/pve-docs/blob/master/generated/qm.1-synopsis.adoc
[qga-protocol]: https://www.qemu.org/docs/master/interop/qemu-ga-ref
[qga-source]: https://raw.githubusercontent.com/qemu/qemu/master/qga/commands.c
[qga-overview]: https://www.qemu.org/docs/master/interop/qemu-ga.html
[proxmox-file-read]: https://github.com/proxmox/qemu-server/blob/master/src/PVE/API2/Qemu/Agent.pm
[qemu-chardev]: https://www.qemu.org/docs/master/system/qemu-manpage.html
[journalctl]: https://www.freedesktop.org/software/systemd/man/journalctl.html
[journal-upload]: https://github.com/systemd/systemd/blob/main/man/systemd-journal-upload.service.xml
[journal-remote]: https://github.com/systemd/systemd/blob/main/man/systemd-journal-remote.service.xml
[otel-agent]: https://opentelemetry.io/docs/collector/deploy/agent/
[otel-journald]: https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/receiver/journaldreceiver/README.md
