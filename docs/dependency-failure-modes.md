# Dependency failure mode analysis — checkoutservice

**Last reviewed:** 2026-09-08
**Owner:** purchase-team
**Control:** RC-066 (Dependency Failure Mode Analysis)
**Risks addressed:** R-029, R-047

`checkoutservice` runs the checkout saga. It is the only service in this
application that moves money, so its upstream failure modes decide whether a
customer can be billed for an order that never ships.

This document records, for each tier-1 upstream, what happens when that
dependency is **slow**, **unavailable**, or **returns unexpected data**, and how
far the failure spreads. The timeout and compensation values in
`src/checkoutservice/main.go` are set from this analysis rather than by guess.

## Saga order and the point of no return

`PlaceOrder` runs these steps in order:

1. `cartservice.GetCart`
2. `productcatalogservice.GetProduct` (once per item)
3. `currencyservice.Convert` (once per item, plus once for shipping)
4. `shippingservice.GetQuote`
5. **`paymentservice.Charge` — the point of no return**
6. `shippingservice.ShipOrder`
7. `cartservice.EmptyCart`
8. `emailservice.SendOrderConfirmation`

Steps 1-4 are safe to fail: nothing has been committed, and the customer sees an
error having lost nothing. Step 5 moves money. **Every failure after step 5 must
either be compensated or recorded**, which is what the reversal path in
`PlaceOrder` and the `orphaned_charge` log record exist to do.

## Per-dependency analysis

### paymentservice (tier 1, critical)

| Scenario | Behaviour | Blast radius |
| --- | --- | --- |
| Slow | `Charge` bounded by `paymentRPCTimeout` (10s). The order fails before the card is charged. | Single checkout. No money moves. |
| Unavailable | `Charge` fails, `PlaceOrder` returns `Internal` before the point of no return. | All checkouts fail; carts and money are untouched. |
| Unavailable *during a reversal* | `Refund` retries `refundAttempts` (3) times inside `refundRPCTimeout` (10s). If every attempt fails, an `orphaned_charge` record is written for manual reconciliation. | One customer billed without a shipment, but the charge is recorded and recoverable. |
| Unexpected data | An empty `transaction_id` is carried into the order record; a reversal for it cannot be correlated. | Single order; caught by reconciliation on the `orphaned_charge`/`charge_reversed` records. |

`Refund` is idempotent on `transaction_id`, so retried reversals cannot refund
the same charge twice. The reversal deliberately runs on
`context.WithoutCancel`: a customer who disconnects mid-checkout must still get
their money back.

### shippingservice (tier 1, critical — the compensation trigger)

| Scenario | Behaviour | Blast radius |
| --- | --- | --- |
| Slow on `GetQuote` | Bounded by `shippingRPCTimeout` (5s). Fails before the charge. | Single checkout, no money moved. |
| Slow on `ShipOrder` | Bounded by `shippingRPCTimeout` (5s). **Card is already charged** — reversal path fires. | One checkout; charge reversed and logged as `charge_reversed`. |
| Unavailable on `ShipOrder` | Same as above: reversal, then `Unavailable` to the caller. | Every checkout fails after charging and reversing. Watch for a reversal storm — a sustained shippingservice outage means a `Refund` for each attempted order. |
| Unexpected data | An empty tracking id reaches the confirmation email. | Cosmetic; the order itself is valid. |

This is the failure that R-029 and R-047 describe. It is the reason the
compensating transaction exists.

### cartservice (tier 1)

| Scenario | Behaviour | Blast radius |
| --- | --- | --- |
| Slow / unavailable on `GetCart` | Bounded by `cartRPCTimeout` (3s). Fails before the charge. | All checkouts fail; nothing is committed. |
| Slow / unavailable on `EmptyCart` | Error is discarded — the order has already succeeded. | The customer keeps a stale cart and may re-order. Accepted: refusing a paid, shipped order would be worse. |
| Unexpected data | An empty cart produces a zero-total order. | Single order. |

### productcatalogservice (tier 1)

| Scenario | Behaviour | Blast radius |
| --- | --- | --- |
| Slow | Bounded by `catalogRPCTimeout` (3s) **per item**. A large cart multiplies the delay. | Single checkout; a big cart can approach the client deadline. |
| Unavailable | Fails before the charge. | All checkouts fail; nothing committed. |
| Unexpected data | A missing or zero price yields a wrong total — and the customer is charged that total. | Single order, but it is a money error. The reversal path is the recovery. |

### currencyservice (tier 1)

| Scenario | Behaviour | Blast radius |
| --- | --- | --- |
| Slow | Bounded by `currencyRPCTimeout` (3s), called once per item plus once for shipping. | Single checkout; the largest source of per-item latency. |
| Unavailable | Fails before the charge. | All non-USD checkouts fail; nothing committed. |
| Unexpected data | A wrong rate charges the wrong amount. | Money error on every order until corrected; reversals are the only remedy. |

### emailservice (tier 2, non-critical)

| Scenario | Behaviour | Blast radius |
| --- | --- | --- |
| Slow / unavailable | Bounded by `emailRPCTimeout` (5s); the error is logged as a warning and the order still succeeds. | Customers get no confirmation email. Deliberate degradation (RC-018). |
| Unexpected data | None — the response is empty. | None. |

## What to watch

These structured records are emitted to Cloud Logging by checkoutservice and
paymentservice. They are the reconciliation trail for money that moved:

| Event | Emitted by | Meaning | Response |
| --- | --- | --- | --- |
| `charge_reversed` | checkoutservice | Charged, shipment failed, charge successfully reversed | No action per order. A rising rate means shippingservice is degraded. |
| `orphaned_charge` | checkoutservice | Charged, shipment failed, **and the reversal also failed** | Refund by hand using `transaction_id`. Should be zero. |
| `refund_processed` | paymentservice | A reversal was issued | Should match `charge_reversed` one-for-one. |
| `refund_replayed` | paymentservice | A reversal was retried and de-duplicated | Expected under retry; a spike means paymentservice is flapping. |

**Not yet built:** a Cloud Logging dashboard and an alert on any
`orphaned_charge`, plus an alert on `charge_reversed` rate. Until those exist,
RC-066's dashboard evidence remains open and detection depends on log search.

## Review

Revisit when the saga gains a step, when a timeout changes, or after any
incident involving a charge without a shipment. Next review due 2027-09-08.
