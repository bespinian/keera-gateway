# Billing

A deployment can hold its own API key for a hosted provider. Every
organisation's models of that provider then use that key, and the deployment
bills each organisation for what it used, at the provider's list price.

This is how a public instance resells a provider. Without such a key, nothing changes: each organisation
enters its own key, as described in [providers.md](providers.md).

With a PostFinance Checkout account, organisations also
[pay in advance](#pay-in-advance), by card.

## Set a provider key

```sh
KEERA_PROVIDER_ANTHROPIC_API_KEY=sk-ant-...
```

The variables are named after the provider: `KEERA_PROVIDER_<NAME>_API_KEY`,
with the name in capitals and hyphens as underscores, such as
`KEERA_PROVIDER_STEPPING_STONE_API_KEY`. `keera model catalogue` lists the
names.

| Setting                                | What it does                                                        |
| -------------------------------------- | ------------------------------------------------------------------- |
| `KEERA_PROVIDER_<NAME>_API_KEY`        | The deployment's key at that provider.                              |
| `KEERA_PROVIDER_INFOMANIAK_PRODUCT_ID` | Required with an Infomaniak key: the product id of your AI Service. |

A misspelt `KEERA_PROVIDER_*` setting stops the start, so a key never quietly
goes unused. So does a product id without its key, or for a provider that takes
none.

## What changes for a model of that provider

- **No key of its own.** The panel hides the provider key field, and the control API
  refuses one. A key the organisation stored before is removed the next time
  the model is saved, and is never sent.
- **The provider's own endpoint.** The backend is always the provider's
  address, whatever the model says. Otherwise an administrator could point the
  model at a server of their own and read the key.
- **The provider's list prices.** The prices are the ones in the provider
  table, including the dearer ones for a long prompt
  ([providers.md](providers.md#long-prompts)). The organisation cannot change
  them. Its budgets and reports use them too.
- **Only models in the catalogue.** A model id the catalogue does not know cannot
  be priced, so it is refused. A model saved before the key was set is sent
  without a key, and the gateway logs why.
- **No hosted tools.** Tools the provider runs itself, such as web search or
  code execution, are removed from every request
  ([mcp.md](mcp.md#hosted-tools)).
- **No dearer modes.** Fields that make the provider charge more than its list
  price are removed from every request: `speed` (Anthropic's fast mode),
  `inference_geo`, and a `service_tier` other than `auto`, `default`, `flex`
  or `standard_only`. The model answers at its normal speed and price. The
  response header `X-Keera-Removed-Fields` names what was removed.
- **Nothing stored at OpenAI**, which every organisation shares
  ([Three request shapes](gateway.md#three-request-shapes)).

Through a router, these apply when any model the request may reach is on the
deployment's key, whichever one answers it.

A subscription model is not affected: each person's own Claude plan pays for it
([subscriptions.md](subscriptions.md)). Self-hosted models are not affected
either.

## What is billed

Every call to such a model is billed, at list price: the request itself, and
the calls a [filter](filters.md) or a [router](routers.md) makes to it. Each
call is one row in `billing_lines`, written in the same batch as the usage
row.

`keera model check`, `keera filter check` and `keera router check` are billed
too, and need credit like a request. They write no usage row. A model check
is billed at its output ceiling, because it does not read the usage record.

Token counts are the provider's own. When a client hangs up, the gateway goes
on reading the answer, for up to 15 minutes, until the provider's usage record
arrives. Otherwise the reasoning a model never streamed would go unbilled. The
request's row then has the real token counts and the provider's status, still
marked as cancelled. For the same reason, a provider gets 15 minutes to start
answering, not `KEERA_UPSTREAM_HEADER_TIMEOUT`.

Cache writes are billed at their own rate, like the rest of Keera's accounting
([providers.md](providers.md#cache-writes)), including Anthropic's dearer
one-hour cache writes.

When no usage record comes back, the call is billed the most it may have used:
its whole body as prompt and its whole output ceiling as answer. This happens
when the answer is too large to read (`KEERA_MAX_RESPONSE_BYTES`), when the
provider takes longer than the gateway waits, or when it sends no record. For
the request's own call, its usage row is marked as an estimate and the gateway
logs a warning. The calls of filters and routers are billed the same way, but
without the mark or the warning.

## The monthly report

```sh
keera billing                    # this month, your organisation
keera billing --month 2026-09
KEERA_ORG=<org> keera billing    # operators: one organisation
```

One row per organisation and provider model, with calls, tokens and the amount
in CHF. With [payments](#pay-in-advance) on, a last column says how much of the
amount was taken from prepaid credit. For an organisation
[billed by invoice](#for-operators) it is the whole amount, as its credit
goes below zero.

Administrators see their own organisation. Operators see every organisation.

The same report is at `GET /control/v1/billing?month=2026-09`, and as CSV with
`&format=csv`. Months are UTC, like budgets.

In the panel it is **Administration → Billing**, with a CSV button. Only
administrators and operators see it, and only on a deployment that holds at
least one provider key.

## Pay in advance

With a PostFinance Checkout account, organisations buy credit in CHF before
they use the deployment's keys. Each call takes its list price from the
credit. When the credit is used up, calls to such models are refused with
`402 credit_exhausted`. So are calls while
[running requests hold](#keep-in-mind) all that is left. The organisation's own models keep working, and so do
subscription models.

### Set it up

1. In PostFinance Checkout, create an application user under
   **Account > Users > Application Users**, and give it a role that may create
   transactions. Write down its user id and authentication key: the key is
   shown only once.
2. Under **Space > Settings > General > Webhook URLs**, add
   `https://<your gateway>/control/billing/postfinance`. Add a webhook listener
   for the entity **Transaction** with the states **Fulfill**, **Failed**,
   **Decline** and **Voided**.
3. Set these, then restart:

```sh
KEERA_BILLING_POSTFINANCE_SPACE_ID=4711
KEERA_BILLING_POSTFINANCE_USER_ID=123456
KEERA_BILLING_POSTFINANCE_AUTH_KEY=...  # the authentication key, as shown
KEERA_BILLING_VAT=8.1                   # optional
```

| Setting                              | What it does                                                                                                    |
| ------------------------------------ | --------------------------------------------------------------------------------------------------------------- |
| `KEERA_BILLING_POSTFINANCE_SPACE_ID` | Your space in PostFinance Checkout. All three settings switch payments on.                                      |
| `KEERA_BILLING_POSTFINANCE_USER_ID`  | The application user's id.                                                                                      |
| `KEERA_BILLING_POSTFINANCE_AUTH_KEY` | The application user's authentication key, in base64 as PostFinance shows it.                                   |
| `KEERA_BILLING_VAT`                  | VAT added on top of each payment, in percent, such as `8.1`. Default 0. It applies to every organisation alike. |
| `KEERA_BILLING_POSTFINANCE_URL`      | The API's address. Default `https://checkout.postfinance.ch`, which serves test spaces too.                     |

Payments also need at least one `KEERA_PROVIDER_<NAME>_API_KEY`, and
`KEERA_PUBLIC_URL` set to an `https` address: PostFinance sends people back
there after they pay. On your own machine, `http://localhost`, any
`*.localhost` name and a loopback address such as `http://127.0.0.1` work too.

A setting that cannot work stops the start: only some of the three
`KEERA_BILLING_POSTFINANCE_*` settings, `KEERA_BILLING_POSTFINANCE_URL` or
`KEERA_BILLING_VAT` without them, a space id that is not a number, an
authentication key that is not base64, a VAT that is not a number from 0 to
100, no provider key, or a `KEERA_PUBLIC_URL` that is not `https`.

### Turning it on

An organisation that never paid has no credit. So when you switch payments on,
every organisation is refused on the deployment's keys until it pays, or until
you [grant it credit](#for-operators).

### Paying

An administrator opens **Billing** in the panel and clicks **Add credit**, or
runs:

```sh
keera billing topup --amount 100 --save-card
```

They pay on PostFinance's payment page, by card, TWINT or PostFinance Pay.
Keera never sees the card. Afterwards PostFinance sends them back to the
panel. The credit is added once PostFinance says the payment went through.

`keera billing credit` shows the balance, the saved card, the automatic
top-up and the latest payments. `keera billing payments` lists the latest 50
payments, or as many as `--limit` says, up to 500.

A payment is from CHF 1 to CHF 10,000, in whole centimes. VAT and a
transaction fee are added on top. The fee is CHF 0.18 plus 1.55% of the
credit, with the 1.55% at least CHF 0.20: CHF 0.38 on CHF 10, CHF 1.73 on
CHF 100. It pays for the payment and is not added to the credit. The panel
shows the VAT, the fee and the total under the amount. Automatic top-ups carry
the fee too.

An organisation somebody created by signing up is limited until its first
payment, which opens the models and MCP servers inside your network and
sandboxes to it. See
[What a new organisation can use](sso.md#what-a-new-organisation-can-use).

### Receipts

Every paid payment has a receipt: the invoice PostFinance makes for it, as a
PDF. Administrators download it in **Billing**, from the button beside the
payment, or with:

```sh
keera billing receipt <payment>   # the id from 'keera billing payments'
```

The control API serves it at `GET /control/v1/billing/payments/<id>/receipt`.

Keera fetches the receipt from PostFinance each time, so it looks the way the
invoice template of your space says. Put your company's address and VAT number
there. A grant has no receipt, because nobody paid for it.

### Automatic top-up

With a saved card, the organisation can top up by itself:

```sh
keera billing auto-topup --below 50 --amount 200
```

When the credit falls below CHF 50, the saved card is charged CHF 200, without
anyone on the payment page. The amount must be at least the threshold, so one
charge lifts the credit above it. A card is charged at most once every 10
minutes, so a burst of traffic is not paid off by a run of charges. Every 30
seconds, one replica checks.

When a charge is declined, no other is tried. The panel shows why. Saving the
automatic top-up again, or a top-up that is paid, tries again. A grant does
not. When PostFinance cannot be reached, the next try is after 10
minutes.

`keera billing auto-topup --off` turns it off and keeps the card.
`keera billing forget-card` removes the card, here and at PostFinance, and
turns the automatic top-up off. Saving a new card replaces the old one.

### For operators

```sh
keera billing grant --org <org> --amount 50 --note "trial"
keera billing invoiced --org <org>
```

A grant adds credit without a payment, for a trial, a refund or a bank
transfer. A negative amount takes credit away. The note is shown to the
organisation. A grant does not lift the limit of an organisation that signed
up; `keera org set <org> --lift-limit` does.

An organisation **billed by invoice** may use the keys without credit. Its use
is still metered, and its credit goes below zero. Bill it from the monthly
report: its "from credit" column is the whole amount, not money already paid. `--off` makes it pay in advance again.

### How a payment is confirmed

PostFinance calls the webhook when a transaction changes. The call proves
nothing, so Keera reads the transaction back over the authenticated API. A
payment counts as paid only in PostFinance's state **Fulfill**, and only with
the amount and currency Keera asked for. A payment Keera already marked failed
is still read, so one paid late is still credited.

A lost webhook does not lose a payment. Opening **Billing** checks at once up
to 5 pending payments the organisation started in the last hour, and every 30
seconds one replica checks those pending for more than a minute, for up to a
day. After that, only the webhook settles a payment. A payment interrupted
before PostFinance made a transaction for it is marked failed after 10 minutes. An automatic top-up still
open after a day is marked failed, and the log names its transaction, so
someone can look it up in PostFinance.

## Keep in mind

- **The list prices are a table in the build.** The comment above it in
  `internal/catalog/providers.go` says when each was last checked. A price
  change takes effect with the next build. Rows already written keep the price
  they were billed at.
- **One key for everyone.** The provider's rate limits apply to all
  organisations together. Give each organisation a budget and rate limits, so
  one cannot use up the key for the rest.
- **Keep the rows.** `KEERA_USAGE_RETENTION` does not delete billing rows or
  payments, and neither does deleting an organisation. That removes only its
  saved card, here and at PostFinance. Back up the database.
- **Running requests hold credit.** A request is charged when it ends. Until
  then, it holds the most it may cost: its whole body as prompt, plus its
  output ceiling (64,000 tokens if it sets none), at the dearest model it may
  reach. Each route step's decision is added twice, since an unreadable answer
  is asked again, and so is a rewrite of the whole body by each filter. A new
  request starts only while some credit is not held. So many
  requests started together cannot all spend the same credit.
- **Credit overshoots a little.** The last request let in may cost more than
  what was free. A request without an output ceiling may write more than it
  holds. Each replica holds only its own requests, and reads the balance every
  `KEERA_SPEND_REFRESH`. So credit can go slightly below zero.
- **A router or filter on the keys counts.** A request needs credit when its
  model, any model its router may choose, the model a route step in it decides
  with, or one of its filters' models is on the deployment's keys.
- **Dropped events are not billed.** If the database is away for longer than
  the usage buffer holds, events are dropped, and their billing rows with them.
  `/readyz` reports the count as `usage_dropped`. Events that carry billing
  rows have a buffer of their own, written first, so a flood of refused
  requests cannot push them out. When Postgres refuses one event, only that
  event is lost: its usage row and daily totals, and, if it has no billing
  rows, what it counted against budgets. The rest of its batch and its own
  billing rows are still written.
