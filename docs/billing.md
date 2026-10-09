# Billing

A deployment can hold its own API key for a hosted provider. Every
organisation's models of that provider then use that key, and the deployment
bills each organisation for what it used, at the provider's list price.

This is how a public instance resells a provider: you buy at a discount and
bill the list price. Without such a key, nothing changes: each organisation
enters its own key, as described in [providers.md](providers.md).

With a PostFinance Checkout account, organisations also
[pay in advance](#pay-in-advance), by card.

## Set a provider key

```sh
KEERA_PROVIDER_ANTHROPIC_API_KEY=sk-ant-...
KEERA_PROVIDER_ANTHROPIC_DISCOUNT=15        # optional, in percent
```

The variables are named after the provider: `KEERA_PROVIDER_<NAME>_API_KEY`,
with the name in capitals and hyphens as underscores, such as
`KEERA_PROVIDER_STEPPING_STONE_API_KEY`. `keera model providers` lists the
names.

| Setting                                | What it does                                                                                                     |
| -------------------------------------- | ---------------------------------------------------------------------------------------------------------------- |
| `KEERA_PROVIDER_<NAME>_API_KEY`        | The deployment's key at that provider.                                                                           |
| `KEERA_PROVIDER_<NAME>_DISCOUNT`       | Your discount on the list price, in percent, such as `12.5`. It sets the provider cost operators see. Default 0. |
| `KEERA_PROVIDER_INFOMANIAK_PRODUCT_ID` | Required with an Infomaniak key: the product id of your AI Service.                                              |

A misspelt `KEERA_PROVIDER_*` setting stops the start, so a key never quietly
goes unused.

## What changes for a model of that provider

- **No key of its own.** The panel hides the API key field, and the control API
  refuses one. A key the organisation stored before is removed the next time
  the model is saved, and is never sent.
- **The provider's own endpoint.** The backend is always the provider's
  address, whatever the model says. Otherwise an administrator could point the
  model at a server of their own and read the key.
- **The provider's list prices.** The prices are the ones in the provider
  table, including the dearer ones for a long prompt
  ([providers.md](providers.md#long-prompts)). The organisation cannot change
  them. Its budgets and reports use them too.
- **Only models in the price table.** A model id the table does not know cannot
  be priced, so it is refused. A model saved before the key was set is sent
  without a key, and the gateway logs why.
- **No hosted tools.** Tools the provider runs itself, such as web search or
  code execution, are removed from every request
  ([mcp.md](mcp.md#hosted-tools)). The provider charges for them on top of
  tokens, and Keera does not bill that.
- **No dearer modes.** Fields that make the provider charge more than its list
  price are removed from every request: `speed` (Anthropic's fast mode),
  `inference_geo`, and a `service_tier` other than `auto`, `default`, `flex`
  or `standard_only`. The model answers at its normal speed and price. The
  response header `X-Keera-Removed-Fields` names what was removed.
- **Nothing stored at OpenAI.** Every organisation shares the deployment's
  OpenAI account. So a Responses request is sent with `store` off, and one with
  `previous_response_id`, `conversation` or `background` is refused with a 400. Send the whole conversation in `input` instead.

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
arrives. Otherwise the reasoning a model never streamed would go unbilled. For
the same reason, a provider gets 15 minutes to start answering, not
`KEERA_UPSTREAM_HEADER_TIMEOUT`.
Cache writes are billed at their own rate, like the rest of Keera's accounting
([providers.md](providers.md#cache-writes)), including Anthropic's dearer
one-hour cache writes.

When no usage record comes back, the call is billed the most it may have used:
its whole body as prompt and its whole output ceiling as answer. This happens
when the answer is too large to read (`KEERA_MAX_RESPONSE_BYTES`), when the
provider takes longer than the gateway waits, or when it sends no record. It
applies to the calls of filters and routers too. The request's usage row is
marked as an estimate, and the gateway logs a warning.

## The monthly report

```sh
keera billing                      # this month, your organisation
keera billing --month 2026-09
keera billing --month 2026-09 --org <id>   # operators
```

One row per organisation and provider model, with calls, tokens and the amount.
Each currency is summed on its own. Anthropic and OpenAI are in USD, the Swiss
providers in CHF. Nothing is converted.

Administrators see their own organisation. Operators see every organisation,
and also the provider cost and the margin. The margin is never shown to anyone
else.

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
KEERA_POSTFINANCE_SPACE_ID=4711
KEERA_POSTFINANCE_USER_ID=123456
KEERA_POSTFINANCE_AUTH_KEY=...          # the authentication key, as shown
KEERA_BILLING_CHF_PER_USD=0.80          # for Anthropic and OpenAI, which price in USD
KEERA_BILLING_VAT=8.1                   # optional
```

| Setting                      | What it does                                                                                                         |
| ---------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `KEERA_POSTFINANCE_SPACE_ID` | Your space in PostFinance Checkout. All three settings switch payments on.                                           |
| `KEERA_POSTFINANCE_USER_ID`  | The application user's id.                                                                                           |
| `KEERA_POSTFINANCE_AUTH_KEY` | The application user's authentication key, in base64 as PostFinance shows it.                                        |
| `KEERA_BILLING_CHF_PER_USD`  | How many francs one dollar takes from the credit. Required when a provider key is for a provider that prices in USD. |
| `KEERA_BILLING_VAT`          | VAT added on top of each payment, in percent, such as `8.1`. Default 0. It applies to every organisation alike.      |
| `KEERA_POSTFINANCE_URL`      | The API's address. Default `https://checkout.postfinance.ch`, which serves test spaces too.                          |

Payments also need at least one `KEERA_PROVIDER_<NAME>_API_KEY`, and
`KEERA_PUBLIC_URL` set to an `https` address: PostFinance sends people back
there after they pay. On your own machine, `http://localhost` works too.

A new exchange rate applies once the gateway restarts with it. Each billing
line keeps the credit it took, so a new rate does not change old lines.

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
top-up and the latest payments. `keera billing payments` lists every payment.

A payment is from CHF 10 to CHF 10,000, in whole centimes. VAT is added on
top.

An organisation somebody created by signing up is limited until its first
payment, which opens the models and MCP servers inside your network and
sandboxes to it. See
[What a new organisation can use](sso.md#what-a-new-organisation-can-use).

### Automatic top-up

With a saved card, the organisation can top up by itself:

```sh
keera billing auto-topup --below 50 --amount 200
```

When the credit falls below CHF 50, the saved card is charged CHF 200, without
anyone on the payment page. The amount must be at least the threshold, so one
charge lifts the credit above it. A card is charged at most once every 10
minutes, so a burst of traffic is not paid off by a run of charges. Each
replica checks every 30 seconds.

When a charge is declined, no other is tried. The panel shows why. Saving the
automatic top-up again, or adding credit by hand, tries again. When
PostFinance cannot be reached, the next try is after 10 minutes.

`keera billing forget-card` removes the card, here and at PostFinance, and
turns the automatic top-up off. Saving a new card replaces the old one.

### For operators

```sh
keera billing grant --org <id> --amount 50 --note "trial"
keera billing invoiced on --org <id>
```

A grant adds credit without a payment, for a trial, a refund or a bank
transfer. A negative amount takes credit away. The note is shown to the
organisation. A grant does not lift the limit of an organisation that signed
up; `keera org set <id> --lift-limit` does.

An organisation **billed by invoice** may use the keys without credit. Its use
is still metered, and its credit goes below zero. Bill it from the monthly
report.

### How a payment is confirmed

PostFinance calls the webhook when a transaction changes. The call proves
nothing, so Keera only takes it as a reason to read the transaction back over
the authenticated API. Only a transaction of a pending or failed Keera payment
is read, so one paid after Keera gave up on it is still credited.
A payment counts as paid in PostFinance's state **Fulfill**, and only if the
amount and currency are the ones Keera asked for.

A lost webhook does not lose a payment. Opening **Billing** looks up the
latest payments at once. Every 30 seconds, each replica also looks up the
payments pending for more than a minute, for a day. A payment still open after
that stays pending, and only its webhook settles it, so one paid late is still
credited. An automatic top-up that has not ended after a day is marked failed,
and the log names its transaction, so someone can look it up in PostFinance.

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
  reach. A new request starts only while some credit is not held. So many
  requests started together cannot all spend the same credit.
- **Credit overshoots a little.** The last request let in may cost more than
  what was free. A request without an output ceiling may write more than it
  holds. Each replica holds only its own requests, and reads the balance every
  `KEERA_SPEND_REFRESH`. So credit can go slightly below zero.
- **A router or filter on the keys counts.** A request needs credit when its
  model, any model its router may choose, or one of its filters' models is on
  the deployment's keys.
- **Dropped events are not billed.** If the database is away for longer than
  the usage buffer holds, events are dropped, and their billing rows with them.
  `/readyz` reports the count as `usage_dropped`. Events that carry billing
  rows have a buffer of their own, written first, so a flood of refused
  requests cannot push them out. When Postgres refuses one event, only that
  event's usage row is lost: the rest of its batch and its own billing rows are
  still written.
