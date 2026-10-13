# 500-peer load-test follow-up

## Avoid redundant provider-status transactions

The 500-peer run recorded 1,412 successful provider-status calls over roughly
57 minutes. Availability signals from installing/removing assignments made each
status immediately due, even when DMS still needed the same accepting state.
Each call enters DMS's broad provider transaction.

The control loop now compares the desired accepting/draining/shutdown state to
the last installed DMS session status. Unchanged notifications update local
scheduling metrics without advancing or postponing the normal status deadline.
Changed eligibility and required reconciliation still cause an immediate report.
Local loss of eligibility still disables claims immediately. Normal periodic
status renewal, token refresh, authority deadlines and drain paths remain active.

The regression test injects a stream of unchanged-eligibility capacity updates.
It fails with the old loop, passes with the new loop, and checks that the periodic
refresh still happens during continuing notifications. Additional cases cover
saturation, regained capacity, data-plane loss, expired authority, draining and
required reconciliation.

## Reconnected-source teardown reproduction

`TestSourceBookingRevocationClosesReconnectedOutboundCircuit` uses three local
libp2p hosts: relay, publisher and consumer. Both clients initially have bookings.
It closes the publisher's first relay connection, reconnects it, opens an outbound
circuit to the still-booked consumer, and verifies an application echo succeeds.
It then removes the publisher's old authority and invokes the same `ClosePeer`
operation used by assignment revocation. The newly connected outbound stream
closes while the consumer's booking remains valid.

This reproduces the proposed **later interruption mechanism**, using an explicit
disconnect and revocation. It does not reproduce the SDK's original negotiation
stall, exercise a real heartbeat HTTP 409, or prove that the dev incident had the
same cause. In dev, a heartbeat 409 coincided with the missing frame, but that
log did not identify the assignment.

No revocation behavior is changed here. Removing `ClosePeer` could preserve
connections/circuits beyond their authority and needs a separate design and
regression coverage. The next diagnostic run should capture connection-close
reasons plus reservation/assignment epochs so the original TCP stall and later
revocation can be attributed independently. The existing handover fix is not
implicated: all 450 planned replacements in the dev run were clean.

```sh
go test ./pkg/node -run TestSourceBookingRevocationClosesReconnectedOutboundCircuit -v
go test ./pkg/app -run 'TestAvailability|TestControlLoopCoalesces' -v
make test
make build
go test -race -p 1 ./...
```

All tests use local fixtures. No SDK, wire contract, credential validation, dev
deployment or capacity configuration changes are part of this patch.
