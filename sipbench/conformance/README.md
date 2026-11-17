# Conformance record

What the edge (Kamailio in front of a FreeSWITCH fleet) should do, case by case, drawn from published test plans
and standards, and whether sipbench checks it yet. Keep it current: a new scenario gets an entry (or extends one),
and a case that becomes covered names its scenario. `go test ./...` in `sipbench/` fails when the record and
`scenarios/` disagree (see `catalog_test.go`).

One file per source:

| File | Source |
|---|---|
| `sipbench.yaml` | Cases sipbench checked before this record existed, written for this edge |
| `i3forum.yaml` | i3 Forum, Technical Interoperability Test Plan, Release 2 (2009): carrier-to-carrier interconnect |
| `etsi-ts-102-027-2.yaml` | ETSI TS 102 027-2: SIP (RFC 3261) conformance test purposes |
| `rfc4475.yaml` | RFC 4475: SIP torture test messages (parser robustness) |
| `impairment.yaml` | Network impairment: loss, jitter, delay, reordering on signaling and media |

## Entry schema

```yaml
source:
  id: i3forum-tip-r2                  # short, stable
  title: Technical Interoperability Test Plan, Release 2
  reference: https://...              # where to find it
  citation: >                         # how we use it: case IDs and our own paraphrase, never the source's text
    Section numbers as IDs; titles and expectations paraphrased.

cases:
  - id: i3f-7.2.10                    # unique across all files; prefixed by source
    ref: "7.2.10"                     # the source's own identifier
    title: DTMF RFC 2833, carrier A to B, G.711   # our paraphrase
    area: dtmf                        # see the list below
    component: edge                   # what is under test: edge | proxy (Kamailio) | freeswitch | operator
    expect: >                         # what our edge should do, in our words
      Digits sent as telephone-events reach the backend with the same digits and durations within 40 ms.
    status: covered                   # covered | partial | planned | not-applicable
    scenarios: [dtmf-both-ways]       # sipbench scenario names (the name: field); required for covered/partial
    gap: ...                          # partial: what the scenarios don't check yet
    milestone: M2                     # planned: when (see the milestones in the top-level README)
    impairment: {loss: 3%, jitter: 30ms}   # optional: network conditions the case runs under
    reason: ...                       # not-applicable: why (e.g. registrar behaviour; the edge is no registrar)
```

`area` is one of: `call-setup`, `call-release`, `error-handling`, `transfer`, `dtmf`, `codec`, `media`, `fax`,
`headers`, `identity` (caller ID, privacy, number formats), `keepalive`, `transport`, `timers`, `parsing`,
`security`, `impairment`, `capacity`, `registration`, `resilience` (drains, rolls, failover).

Statuses:
- **covered**: a scenario checks what `expect` says.
- **partial**: a scenario checks some of it; `gap` says what's missing.
- **planned**: worth checking on this edge; `milestone` says when.
- **not-applicable**: doesn't apply to this edge (a registrar test, an ISUP gateway test); `reason` says why.

Sources' text stays out of this repository: entries carry the source's identifiers and our own paraphrase.
