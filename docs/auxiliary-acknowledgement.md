# First auxiliary acknowledgement — additive protocol 1 extension

A first summary on a separate provider route has no primary binding. It must not
create one merely to satisfy a client acknowledgement check. Pacer advertises
`auxiliary_ack: {version: 1, kinds: ["compaction", "title", "generate"]}` in its
public protocol-1 discovery, alongside `pacer:true` and unchanged `mode:"off"`.

For an admitted non-primary claim **without an existing primary binding**, both
stream and nonstream responses carry `X-Quota-Protocol: 1` and `X-Quota-Auxiliary`,
unpadded base64url JSON with exactly:

```json
{"protocol":1,"kind":"compaction","request":"the-exact-claim-request-id"}
```

There is no binding, generation, alias, capability, caller ID or checkpoint in this
acknowledgement. The client accepts it only after discovering this Pacer extension,
only for the advertised non-primary kind, and only with an exact request/kind match.
Malformed, missing, mismatched or oversized acknowledgements remain failures.
This never permits a primary without `X-Quota-Binding`, and never substitutes for
a persisted Core checkpoint, a cache-policy receipt, or native policy commit.
Auxiliary-only replies cannot change primary affinity, generation, alias or warmth.

Existing bound auxiliary routes retain their original binding acknowledgement.
Older hosts/legacy quota-handoff do not gain a relaxed acknowledgement check.
Older clients fail closed on an unbound auxiliary rather than receive a fabricated
binding. Pacer strips acknowledgement headers supplied by upstream before applying
its own, and removes all client quota-coordination headers before upstream dispatch.

The motivating native failure and successful same-route recovery remain recorded
in the client's `native-full` evidence. Source tests are not proof of the new
cross-provider native/Core path; that path requires a fresh isolated TUI rerun.
