-- The schema, whole.
--
-- Three things live here. The tenancy model - who exists, what they are
-- allowed and what it costs them. The hooks a request passes through on its
-- way to a model. And the machines the gateway lends out.

-- ---------------------------------------------------------------------------
-- Tenancy
-- ---------------------------------------------------------------------------

CREATE TABLE orgs (
    id         text PRIMARY KEY,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- Maps an identity provider's email domain to a tenant, so a first login
    -- lands in the right organisation without anybody being invited by hand.
    -- Null in a dedicated or on-premises deployment, where there is only one
    -- organisation.
    email_domain text
);
CREATE UNIQUE INDEX orgs_email_domain_key
    ON orgs (lower(email_domain)) WHERE email_domain IS NOT NULL;

CREATE TABLE teams (
    id         text PRIMARY KEY,
    org_id     text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

CREATE TABLE users (
    id          text PRIMARY KEY,
    org_id      text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    email       text NOT NULL,
    -- Subject claim from the customer's identity provider, prefixed with the
    -- provider that vouched for it as "<provider>:<subject>". A subject is
    -- only ever unique within the directory that issued it, and the hosted
    -- deployment signs in customers whose directories are not the same
    -- directory. Null for users Keera authenticates itself.
    external_id text,
    -- Roles are deliberately few. "operator" is us, and crosses organisations;
    -- "admin" runs one organisation's teams, guardrails and keys; "member" can
    -- see what they and their own teams are using and nothing else.
    role        text NOT NULL DEFAULT 'member'
        CHECK (role IN ('operator', 'admin', 'member')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- A person can be disabled, for when they leave.
    --
    -- Disabled rather than deleted: usage rows, keys, sandboxes and audit
    -- entries still name them, and "whose was this" has to keep an answer. A
    -- disabled person cannot sign in, and their keys are revoked when they are
    -- disabled.
    disabled_at timestamptz,
    UNIQUE (org_id, email)
);
CREATE UNIQUE INDEX users_external_id_key ON users (external_id) WHERE external_id IS NOT NULL;

CREATE TABLE api_keys (
    id         text PRIMARY KEY,
    org_id     text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    team_id    text REFERENCES teams (id) ON DELETE CASCADE,
    user_id    text REFERENCES users (id) ON DELETE SET NULL,
    -- The label on a key is not what the key is called; it is what stands in
    -- for the key wherever the key itself must not appear. The secret is shown
    -- once and never again, so every screen, report and audit entry that has
    -- something to say about a key says it about this column instead - and a
    -- rotation deliberately carries the label over to a different credential,
    -- which is a thing an alias does and a name does not.
    alias      text NOT NULL,
    -- SHA-256 of the presented key. The key itself is never stored.
    key_hash   bytea NOT NULL UNIQUE,
    prefix     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    revoked_at timestamptz
);
CREATE INDEX api_keys_org_id_idx ON api_keys (org_id);
CREATE INDEX api_keys_team_id_idx ON api_keys (team_id);

-- What a scope is allowed, at each of the three levels a request belongs to.
--
-- The levels combine restrict-only - minimum for a ceiling, intersection for a
-- list - so delegating team administration cannot be used to grant that team
-- more than its organisation allowed.
CREATE TABLE guardrails (
    scope_type        text NOT NULL CHECK (scope_type IN ('org', 'team', 'key')),
    scope_id          text NOT NULL,
    allowed_models    text[],
    max_output_tokens int,
    rpm               int,
    tpm               int,
    budget_micros     bigint,
    budget_period     text CHECK (budget_period IN ('day', 'month')),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    -- Every other column here narrows a number, and combining two of them is a
    -- minimum. This one carries text and combines by concatenation, so it
    -- needs no CHECK to keep a team inside its organisation: a team's prompt is
    -- sent after the organisation's, never instead of it. Its length is bounded
    -- by the control plane, which is the layer that knows the text is charged
    -- as input tokens on every request the scope makes.
    system_prompt     text,

    -- Which filters this scope applies, in the order they run.
    --
    -- Like system_prompt and unlike every other column here, this one adds
    -- rather than narrows: a team's filters run after the organisation's, never
    -- instead of them. An alias that no filter in the scope's organisation
    -- answers to is a refusal at request time and not a silently skipped
    -- guardrail - a filter that can be removed by deleting it is not a
    -- guardrail.
    filters           text[],

    -- The sandbox half. Each of these is here because leaving it out has a
    -- failure somebody has actually had: a cluster full of machines nobody
    -- deleted, the same thing more slowly, a department on the frontier-model
    -- budget also being on the sixty-four-core class, and one team's agent
    -- fleet taking a node pool three other teams share.
    max_sandboxes           integer,
    max_sandbox_ttl_seconds integer,
    sandbox_classes         text[],
    max_sandbox_cpu_millis  integer,
    max_sandbox_memory_mib  integer,

    -- Which tools a scope may call, and whether a hosted provider's own tools
    -- are taken out of its requests.
    --
    -- allowed_tools narrows like allowed_models: an entry is a server's alias,
    -- for all of its tools, or 'alias/tool' for one, and a level can only
    -- narrow what it inherits. block_hosted_tools is a switch that, once on at
    -- some level, stays on below it.
    allowed_tools      text[],
    block_hosted_tools boolean,

    PRIMARY KEY (scope_type, scope_id)
);

-- ---------------------------------------------------------------------------
-- The catalogue
-- ---------------------------------------------------------------------------

CREATE TABLE models (
    alias                  text PRIMARY KEY,
    kind                   text NOT NULL CHECK (kind IN ('chat', 'completion', 'embedding')),
    backends               text[] NOT NULL,
    backend_model          text NOT NULL,
    input_micros_per_mtok  bigint NOT NULL DEFAULT 0,
    output_micros_per_mtok bigint NOT NULL DEFAULT 0,
    -- What the provider served from its own prompt cache is billed at a
    -- fraction of the input price - a tenth at OpenAI and at Anthropic, less on
    -- some models. Zero is "not stated", which the gateway charges at the full
    -- input price. See policy.Model.cachedInputMicros for why this one price
    -- reads a zero that way.
    cached_input_micros_per_mtok bigint NOT NULL DEFAULT 0,
    max_context            int NOT NULL DEFAULT 0,
    -- Names an environment variable, never a secret. It is still the right
    -- answer for a deployment whose secrets come from a Kubernetes Secret or a
    -- vault, and it is how one credential is shared by several models.
    api_key_env            text NOT NULL DEFAULT '',
    -- A credential for somebody else's endpoint, so that an operator can add a
    -- hosted model from the control panel instead of needing an environment
    -- variable and a restart. Encrypted with a key that lives only in the
    -- gateway's environment (KEERA_SECRET_KEY), authenticated with the alias of
    -- the model it belongs to, and never returned by the control API.
    api_key_ct             bytea,
    -- What this model is for, in a sentence.
    --
    -- A router's decision is which of several models suits a prompt, and the
    -- only things it could otherwise be told about the candidates are their
    -- aliases and their prices. The description belongs to the model, is
    -- written once by whoever put the model in the catalogue, and is handed to
    -- every router that offers it. It is also served on /v1/models, because a
    -- client choosing between the aliases it can see has the same question a
    -- router does.
    description            text NOT NULL DEFAULT '',
    -- Which hosted provider this model was declared against. A provider is a
    -- table of defaults: naming one fills in the backend URL, the credential
    -- variable, the context window and the prices, and the entry keeps the
    -- expanded values rather than the name. Keeping the name is what says
    -- whether those values are the provider's answers or the operator's - the
    -- difference between a form that shows them and a form that asks somebody
    -- to check somebody else's homework. Empty is a model served by an
    -- inference plane of the deployment's own.
    provider               text NOT NULL DEFAULT '',
    -- Declared in KEERA_MODELS_FILE, which is applied on every start. Rather
    -- than let a panel or a CLI accept a change it cannot keep, the ones the
    -- file declares are marked here and refused to every other writer.
    -- Applying a catalogue sets the flag; applying one that no longer names a
    -- model clears it, which is how an entry deleted from the file is handed
    -- back to the operator.
    managed                boolean NOT NULL DEFAULT false,
    enabled                boolean NOT NULL DEFAULT true,
    updated_at             timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Hooks: what a request passes through
-- ---------------------------------------------------------------------------

-- Filters: something every prompt passes through before it is forwarded.
--
-- The case this exists for is a prompt that is about to leave the cluster. An
-- organisation that allows a hosted model at all wants what reaches it to have
-- had its credentials, its customer names and its client data taken out first.
--
-- A filter belongs to an organisation, unlike the model catalogue which is
-- shared by every tenant. The model it names is the operator's; the wording is
-- the organisation's, and it is the wording that encodes which of that
-- organisation's data is the data that must not leave.
--
-- The alias is the identity, as a model's is, because it is what an
-- administrator types into a guardrail rather than something they copy.
CREATE TABLE filters (
    org_id      text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    alias       text NOT NULL,
    -- The model this filter runs on. Deliberately not a foreign key into
    -- models: the catalogue is shared and a filter is not, so a model must
    -- not become undeletable because one tenant referred to it. A filter whose
    -- model has gone refuses the requests it guards, which is the safe
    -- direction and is visible on the panel.
    model       text NOT NULL,
    prompt      text NOT NULL,
    description text NOT NULL DEFAULT '',

    -- What the filter does with the request it has read.
    --
    -- A 'rewrite' filter answers with the whole conversation written back out.
    -- That is the only way to take a credential out of a prompt and still let
    -- the prompt go, and it is why it costs a second generation as long as the
    -- request on every call.
    --
    -- A 'gate' answers with one word. It cannot redact anything and never edits
    -- what the client sent - it reads the request and says whether it may go -
    -- so it costs a verdict instead of a conversation. It is the more reliable
    -- of the two at the job it has: a small model asked for a verdict is not
    -- also holding a format that requires it to copy a page of text back
    -- verbatim.
    --
    -- A 'pattern' filter is a list of rules and no model at all. Much of what a
    -- redaction filter removes is not a judgement: an API key, a connection
    -- string, an IBAN, a card number are each a shape, and against a shape a
    -- regular expression is faster, cheaper and more reliable than any model.
    -- It also cannot commit the mistake docs/filters.md warns about throughout
    -- - a rule never reformats a stack trace or rewrites somebody's source,
    -- because it does not know what either is. What it cannot do is read, so it
    -- replaces neither of the other modes.
    mode        text NOT NULL DEFAULT 'rewrite'
        CHECK (mode IN ('rewrite', 'gate', 'pattern')),

    -- Shadow: the filter runs and enforces nothing.
    --
    -- It is a flag rather than a fourth mode because the mode says what the
    -- filter is asked to produce, and shadowing must not change that. The point
    -- of a week in shadow is that what was measured is what gets turned on.
    --
    -- A shadow filter is not a guardrail, and the one place that shows is
    -- failure. Everywhere else a filter that cannot run refuses the request,
    -- because a control that can be turned off by breaking it is not a control.
    -- A shadow filter that cannot run records that it could not and lets the
    -- request past: it is a measurement, and a measurement that takes a
    -- department offline is worse than no measurement.
    shadow      boolean NOT NULL DEFAULT false,

    -- A pattern filter's rules. JSONB rather than their own table: read and
    -- written as a unit, never queried across filters, and their order is part
    -- of what they mean.
    rules       jsonb NOT NULL DEFAULT '[]'::jsonb,

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, alias)
);

-- What each mode must and must not carry.
--
-- The emptiness is enforced rather than tolerated: a model left behind by a
-- mode change is what an administrator would go on reading off the filter's
-- screen as the thing deciding its answers.
--
-- A pattern filter needs at least one rule. One with none reads every request
-- the guardrail covers and has nothing to say about any of them, which is not a
-- filter that passes everything - it is a filter somebody thinks is working.
ALTER TABLE filters
    ADD CONSTRAINT filters_mode_fields CHECK (
        (mode = 'pattern'
            AND model = '' AND prompt = ''
            AND jsonb_typeof(rules) = 'array' AND jsonb_array_length(rules) > 0)
        OR (mode <> 'pattern'
            AND model <> '' AND prompt <> ''
            AND rules = '[]'::jsonb)
    );

-- Routers: something that reads a request and changes where it goes.
--
-- A filter is a hook that reads a request and changes it. The case a router
-- exists for is the one a deployment with more than one model always ends up
-- having: most requests do not need the largest model in the catalogue, and
-- some of them must not reach the smallest one because the smallest one is the
-- only one outside the cluster.
--
-- A router is named by the client, in the 'model' field, exactly as a model is.
-- That is the whole of how a request reaches one, and it is deliberate: a hook
-- that rerouted a request which had asked for a particular model would hand a
-- developer an answer from a model they did not choose and no way to tell.
-- Naming the router is asking to be routed. An administrator who wants a scope
-- routed whatever it asks for has the allow-list already: a key whose
-- allowed_models is just the router can reach nothing else.
--
-- A router belongs to an organisation, as a filter does, and for the same
-- reason: the models are the operator's and the judgement about which requests
-- deserve which of them is the organisation's.
--
-- Its alias shares a namespace with the catalogue's: a client puts one string
-- in the 'model' field and the gateway decides, from that string alone, whether
-- it named a model or a router.
CREATE TABLE routers (
    org_id      text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    alias       text NOT NULL,
    -- The model that makes the decision, for an instruction router.
    -- Deliberately not a foreign key into models, for the reason filters.model
    -- is not: the catalogue is shared and a router is not, so one tenant naming
    -- a model must not make it undeletable. A router whose model has gone falls
    -- back or refuses, which is visible on its own screen.
    model       text NOT NULL,
    prompt      text NOT NULL,
    -- The models this router may choose between, in the order they are offered
    -- to it. A destination that is not in this list cannot be reached through
    -- this router however the decision is worded, which is what makes the list
    -- and not the instruction the thing an administrator reads to know where a
    -- router can send a prompt.
    destinations text[] NOT NULL,
    -- Where a request goes when the decision could not be made: the router's
    -- model is gone, or answered with something that names no destination.
    --
    -- NULL is a router that refuses instead, and the choice belongs to the
    -- router because the two kinds of router want opposite answers. A router
    -- that exists to save money has a safe answer available - the small model,
    -- or the large one - and refusing the request would take a department
    -- offline over a dispatch decision. A router that exists to keep some
    -- prompts inside the cluster has no safe answer: sending an unclassified
    -- prompt to whichever model was named in a column is the failure the
    -- router was bought to prevent.
    fallback    text,
    description text NOT NULL DEFAULT '',

    -- What chooses the model that answers.
    --
    -- 'instruction' is a small model and a prompt, reading each request and
    -- naming the model that should answer it. It answers "which of these suits
    -- this request?".
    --
    -- 'fallback' tries the destinations in the order they are written and is
    -- answered by the first one that answers. It answers "which of these is
    -- up?" - a local cluster with a hosted endpoint behind it, or two providers
    -- of the same frontier model. Nothing reads the request.
    --
    -- 'latency' and 'least-busy' take the same written list and put it in an
    -- order the gateway works out for itself, request by request, for the
    -- routers whose destinations are equals. Two identical vLLM deployments of
    -- the same model are not a first choice and a second choice, and a router
    -- that sent every request to the first of them would be a deployment
    -- running on half its GPUs while looking, from every dashboard and every
    -- check, like one that works. 'latency' ranks by what each destination has
    -- lately taken to begin answering, which is the one to reach for when the
    -- destinations are not alike, because a queue depth cannot be compared
    -- across machines of different speeds. 'least-busy' ranks by requests in
    -- flight, which is the one to reach for when the destinations are alike and
    -- the requests are not.
    --
    -- What those two measure lives in the gateway process and is written
    -- nowhere: it is a few minutes of one replica's traffic, worth nothing once
    -- it is stale and nothing to a different replica. A measured router with
    -- nothing measured yet is a fallback router - the written order is the
    -- tie-break at every level. See internal/gateway/load.go.
    --
    -- 'size' chooses by how much text is in the request. It costs nothing, it
    -- answers the same way twice, and it cannot be argued with: a prompt asking
    -- for the large model is a prompt four words longer, where docs/routers.md
    -- has to warn that an instruction router's decision is the least
    -- trustworthy input on the path. It is also the one mode answering a
    -- question of correctness rather than of price - a request that will not
    -- fit through the small model's context has to go somewhere larger, so the
    -- gateway ranks a destination that cannot hold it behind every one that
    -- can, whatever the ceilings say.
    mode        text NOT NULL DEFAULT 'instruction'
        CHECK (mode IN ('instruction', 'fallback', 'latency', 'least-busy', 'size')),

    -- A size router's per-destination ceilings. JSONB for the reason the
    -- filters' rules are, and a map keyed by alias rather than an array
    -- parallel to destinations: two arrays that have to stay the same length
    -- are two arrays that eventually do not.
    ceilings    jsonb NOT NULL DEFAULT '{}'::jsonb,

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, alias)
);

-- What each mode requires, said here because the modes need opposite things of
-- the same columns and a row that has neither is a router that cannot place
-- anything.
--
-- An instruction router has to have a model to decide with and something to
-- tell it. Every mode that reads nothing has to have neither, and the emptiness
-- is enforced rather than ignored: a deciding model left behind by a mode
-- change is a model an administrator would go on reading off the router's
-- screen as the thing choosing its destinations, which nothing would be.
--
-- The fallback column goes the same way. A router with no decision to fail to
-- make has no use for it - its whole list is the fallback, since the next
-- destination is what happens when one fails.
ALTER TABLE routers
    ADD CONSTRAINT routers_mode_fields CHECK (
        (mode = 'instruction'
            AND model <> '' AND prompt <> '' AND ceilings = '{}'::jsonb)
        OR (mode = 'size'
            AND model = '' AND prompt = '' AND fallback IS NULL
            AND jsonb_typeof(ceilings) = 'object')
        OR (mode NOT IN ('instruction', 'size')
            AND model = '' AND prompt = '' AND fallback IS NULL
            AND ceilings = '{}'::jsonb)
    );

-- MCP servers: the tools an agent calls, governed like the models it calls.
--
-- A coding agent spends as much of its time calling tools as calling models,
-- and a tool call is where data leaves for somewhere other than a model: an
-- issue tracker, a chat channel, a cloud API. The gateway stands in front of
-- the MCP servers that carry those calls, so the same keys, allow-lists and
-- filters decide them, and the same log records them.
--
-- Shared by every tenant, like models: the operator declares where a server is
-- and what credential it takes, and a guardrail decides which of its tools a
-- scope may call.
CREATE TABLE mcp_servers (
    alias        text PRIMARY KEY,
    -- The server's Streamable HTTP endpoint.
    url          text NOT NULL,
    description  text NOT NULL DEFAULT '',
    -- The header the credential goes in. Empty is Authorization, as a bearer
    -- token, which is what most servers read.
    auth_header  text NOT NULL DEFAULT '',
    -- As on models: an environment variable's name, or a sealed credential
    -- set from the control plane and never returned by it.
    api_key_env  text NOT NULL DEFAULT '',
    api_key_ct   bytea,
    managed      boolean NOT NULL DEFAULT false,
    enabled      boolean NOT NULL DEFAULT true,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- What happened: usage, filter runs, spend, audit
-- ---------------------------------------------------------------------------

CREATE TABLE usage_events (
    id                bigserial PRIMARY KEY,
    ts                timestamptz NOT NULL DEFAULT now(),
    org_id            text NOT NULL,
    team_id           text,
    user_id           text,
    key_id            text,
    -- The model that answered, which is where every other number on this row
    -- comes from.
    alias             text NOT NULL,
    input_tokens      int NOT NULL DEFAULT 0,
    output_tokens     int NOT NULL DEFAULT 0,
    -- The part of the prompt the provider served from its own cache, kept so a
    -- row can be reconciled against an invoice rather than only believed.
    cached_input_tokens int NOT NULL DEFAULT 0,
    cost_micros       bigint NOT NULL DEFAULT 0,
    status            int NOT NULL,
    latency_ms        int NOT NULL,
    -- Time to the first token the client saw. For a coding agent this is the
    -- latency a developer perceives; the total is dominated by output length.
    ttft_ms           int NOT NULL DEFAULT 0,
    stream            boolean NOT NULL DEFAULT false,
    -- True when the token counts are inferred rather than reported, which
    -- happens when a client disconnects before the upstream usage record.
    estimated         boolean NOT NULL DEFAULT false,
    canceled          boolean NOT NULL DEFAULT false,

    -- What the caller was told, which is either the inference plane's own
    -- wording carried through unchanged or, for a refusal, the guardrail's. A
    -- status says a request failed; it does not say that the backend refused
    -- the sampling parameters, that the context window was exceeded, or that
    -- one of two endpoints behind a model is refusing every connection - and
    -- those are the three questions somebody looking at "12 failed" on the
    -- dashboard actually has. The gateway bounds its length before it is
    -- written: the upstream half of it is somebody else's free text.
    error             text,

    -- The conversation this request belongs to.
    --
    -- A request is not what anybody does: a developer gives an agent a task and
    -- the agent makes forty calls carrying it out. "What did that cost" and
    -- "why did that take twenty minutes" are questions about the forty.
    --
    -- It is a hash the gateway computes from the conversation the client sent -
    -- not the conversation itself. Nothing about a prompt is stored here and
    -- nothing about one can be recovered from this column, which is the only
    -- way a column derived from source code belongs in a product whose whole
    -- promise is that the source code stays put.
    --
    -- Null is a request that belongs to no conversation: an embedding, or a
    -- body that was refused before it could be read.
    session_key       text,

    -- What the client called itself: the X-Keera-Client header where one is
    -- sent, the User-Agent's product token otherwise, reduced to a short
    -- lowercase name. Nothing is enforced by it and nothing is decided on it -
    -- it is the client's own claim about itself, exactly as a User-Agent is -
    -- so it is a plain nullable column rather than a foreign key to a table of
    -- known clients: an editor released next month has to be able to appear on
    -- the map without a migration, and null is a client that named itself in no
    -- way at all.
    client            text,

    -- Which router chose this request's destination, and what it cost to
    -- choose. One column each rather than a table of runs, which is where the
    -- filter log had to go: a request passes through as many filters as its
    -- guardrails name and through exactly one router, because naming the router
    -- is how it got here. What the router's own generation cost is folded into
    -- cost_micros, as a filter's is.
    router            text,
    router_outcome    text,
    router_ms         integer NOT NULL DEFAULT 0,

    -- The breakdown of this request's own latency: the filter chain that ran a
    -- second model over the conversation, the router that spent four hundred
    -- milliseconds deciding, the fallback that waited out a dial timeout
    -- against a dead endpoint - or the model itself, in which case there is
    -- nothing here to fix.
    --
    -- A column rather than a table, unlike filter_runs below, because these
    -- rows are never read except through the one request they describe.
    -- filter_runs exists to be aggregated across requests and a table earns its
    -- indexes there. Nothing aggregates a span: the question is always about
    -- one request that somebody is looking at, and a join per row read would be
    -- the cost of a query nobody makes.
    --
    -- What it costs is a few hundred bytes on the busiest table in the schema,
    -- paid on every request, which is why the spans are kept as terse as they
    -- are. Retention sweeps them out with the row they are on.
    spans             jsonb
);
CREATE INDEX usage_events_ts_idx ON usage_events (ts);
CREATE INDEX usage_events_org_ts_idx ON usage_events (org_id, ts);
CREATE INDEX usage_events_team_ts_idx ON usage_events (team_id, ts);
CREATE INDEX usage_events_key_ts_idx ON usage_events (key_id, ts);
-- The per-entity reports: one team, one key or one model, read on its own
-- screen rather than as a share of the organisation's total.
CREATE INDEX usage_events_alias_ts_idx ON usage_events (alias, ts);
CREATE INDEX usage_events_user_ts_idx ON usage_events (user_id, ts);

-- The live request log's own shape. It filters on the organisation and orders
-- by id - `WHERE org_id = $1 AND id > $after ORDER BY id DESC LIMIT n` - and
-- the stream behind it deliberately sends no time window at all, because a row
-- arriving live is inside every window the screen offers and pinning the end
-- would stop the stream a second after it opened. usage_events_org_ts_idx leads
-- with the organisation but orders by ts, which answers the reports and not
-- this.
CREATE INDEX usage_events_org_id_idx ON usage_events (org_id, id DESC);

-- Failures are a small fraction of the log and are always read newest first, so
-- the index over them is partial and ordered. The predicate matches the one the
-- failure report filters on, including the rows that carry a message without a
-- failing status: a stream that ended early answered 200 and still did not
-- deliver what was asked for.
CREATE INDEX usage_events_failed_idx ON usage_events (org_id, id DESC)
    WHERE status >= 400 OR error IS NOT NULL;

-- A router's own screen reads its rows by organisation and alias over a window,
-- which is the shape the usage log is already indexed for by ts - but a router
-- is a small fraction of an organisation's traffic and this would otherwise
-- scan all of it.
CREATE INDEX usage_events_router_idx ON usage_events (org_id, router, ts DESC)
    WHERE router IS NOT NULL;

-- Reading one session is the opposite shape from listing them. Listing a window
-- is served by the time indexes above - the rows in scope are read by ts and
-- then sorted by conversation - because a report over a month of an
-- organisation's traffic touches most of that window whichever way it is
-- approached. The run-start lookup and the calls themselves want exactly one
-- conversation's rows in time order, which this turns into a single range scan
-- whose cost does not follow how much traffic the deployment has.
--
-- Partial, because the rows with no session_key are the ones a session query
-- never looks at.
CREATE INDEX usage_events_session_idx ON usage_events (org_id, session_key, ts)
    WHERE session_key IS NOT NULL;

-- One row per filter per request it ran on.
--
-- Not a column on usage_events, because a request passes through as many
-- filters as its guardrails name and each of them has its own outcome, its own
-- latency and its own cost. Not a foreign key to usage_events either: the
-- gateway writes usage rows in batches and never learns the ids Postgres gave
-- them, so a filter run carries the same scope columns as the request it
-- belonged to and is read on its own terms.
--
-- Nothing here comes from the request's text. What a filter was shown and what
-- it wrote back are the client's own prose, and this product's whole promise is
-- that those stay where they were sent; what is kept is what the filter did -
-- how many segments it was given, how many it changed, and whether it would
-- have let the request go. That is enough to tune an instruction against a week
-- of real traffic and not enough to reconstruct a word of it.
CREATE TABLE filter_runs (
    id          bigserial PRIMARY KEY,
    ts          timestamptz NOT NULL DEFAULT now(),
    org_id      text NOT NULL,
    -- The filter's alias, and deliberately not a foreign key into filters. A
    -- filter's traffic is its history, and history that disappears when
    -- somebody deletes the row it was about cannot answer "what was this doing
    -- before we removed it".
    filter      text NOT NULL,
    -- The mode and the shadow flag as they were at the moment of the run. A
    -- filter promoted out of shadow on Wednesday should not have Monday's runs
    -- redescribed as enforcement that never happened.
    mode        text NOT NULL,
    shadow      boolean NOT NULL DEFAULT false,
    -- What it did. 'pass' is a request the filter let through untouched - a
    -- gate's allow, or a rewrite that changed nothing. 'rewrite' changed at
    -- least one segment. 'refuse' is the filter saying no, which for a shadow
    -- run is the refusal that did not happen. 'error' is a filter that could
    -- not run at all, which when it is enforcing means the request was refused
    -- for it.
    outcome     text NOT NULL
        CHECK (outcome IN ('pass', 'rewrite', 'refuse', 'error')),
    -- What the run added to the request that was waiting for it, and what it
    -- spent. Both are per run rather than per request: the request's own row
    -- carries the total, and the total cannot say which of three filters is
    -- the expensive one.
    latency_ms  int NOT NULL DEFAULT 0,
    cost_micros bigint NOT NULL DEFAULT 0,
    -- How much of the request the filter was given and how much of it came back
    -- changed. Counts, not text: "this instruction rewrites four segments in
    -- five of every request it sees" is the number that says an instruction is
    -- too eager, and it can be read without keeping any of what was rewritten.
    segments    int NOT NULL DEFAULT 0,
    changed     int NOT NULL DEFAULT 0,
    -- Whose request it was, copied from the same event the request row carries,
    -- so a refusal rate can be broken down by the department that is living
    -- with it. A guardrail that refuses four percent of an organisation's
    -- traffic and sixty percent of one team's is two very different filters,
    -- and only the second number gets anybody's attention.
    team_id     text,
    user_id     text,
    key_id      text,
    -- The model the request was addressed to, which is what says whether a
    -- filter is guarding the hosted model it was written for or has quietly
    -- ended up in front of everything.
    alias       text NOT NULL DEFAULT ''
);

-- One filter's own screen: its runs inside a window, in time order. Every
-- reading this table has - the totals, the chart, the breakdown by team - is
-- that same range scan, which is why there is one index and not four.
CREATE INDEX filter_runs_org_filter_ts_idx ON filter_runs (org_id, filter, ts);
-- The Filters list asks the same question of every filter at once, so it reads
-- the window rather than one alias inside it.
CREATE INDEX filter_runs_org_ts_idx ON filter_runs (org_id, ts);
-- Retention deletes by age across every tenant at once, and neither index above
-- can serve a bare ts < cutoff.
CREATE INDEX filter_runs_ts_idx ON filter_runs (ts);

-- One row per tool call, the tool-side twin of usage_events.
--
-- Like usage_events it holds no content: not the arguments, not the result.
-- Their sizes are kept, because "this tool sent out four megabytes" is worth
-- knowing without knowing what they were.
CREATE TABLE tool_calls (
    id           bigserial PRIMARY KEY,
    ts           timestamptz NOT NULL DEFAULT now(),
    org_id       text NOT NULL,
    team_id      text,
    user_id      text,
    key_id       text,
    -- The server's alias and the tool's name, as the client called them. Not
    -- foreign keys: a server's history outlives the server.
    server       text NOT NULL,
    tool         text NOT NULL,
    -- 'ok' is a result; 'tool_error' a result the tool itself marked as a
    -- failure; 'error' a call that got no result, from the server or the
    -- gateway; 'denied' a tool the key may not call; 'refused' a call a filter
    -- stopped.
    outcome      text NOT NULL
        CHECK (outcome IN ('ok', 'tool_error', 'error', 'denied', 'refused')),
    latency_ms   int NOT NULL DEFAULT 0,
    arg_bytes    int NOT NULL DEFAULT 0,
    result_bytes int NOT NULL DEFAULT 0,
    -- What the filters on the arguments spent. Charged to the same budgets as
    -- a request's filters.
    cost_micros  bigint NOT NULL DEFAULT 0,
    error        text,
    -- A session the client named in a header; tool calls cannot be matched to
    -- a conversation any other way. See docs/mcp.md.
    session_key  text,
    client       text
);

-- The tool calls of one organisation inside a window, which every reading of
-- this table is.
CREATE INDEX tool_calls_org_ts_idx ON tool_calls (org_id, ts);
-- The calls of one key inside a window, which is how a session's calls are
-- found when the client named none.
CREATE INDEX tool_calls_key_ts_idx ON tool_calls (key_id, ts);
-- Retention deletes by age across every tenant at once.
CREATE INDEX tool_calls_ts_idx ON tool_calls (ts);

-- Rolled up alongside usage_events so a budget check is one indexed read rather
-- than an aggregate over the event log.
CREATE TABLE spend (
    scope_type   text NOT NULL,
    scope_id     text NOT NULL,
    period       text NOT NULL,
    period_start date NOT NULL,
    micros       bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (scope_type, scope_id, period, period_start)
);

-- The budget refresh reads the two currently open windows every few seconds, in
-- every replica, and the primary key leads with the scope. What makes that
-- worth an index rather than leaving it to a small table is that the table is
-- only small where retention is configured, and retention is off by default:
-- without it spend gains a row per scope per period per day and never loses
-- one, so the cost of the refresh grows with the age of the deployment while
-- the work it actually does stays the same size.
CREATE INDEX spend_window_idx ON spend (period, period_start);

CREATE TABLE audit_log (
    id          bigserial PRIMARY KEY,
    ts          timestamptz NOT NULL DEFAULT now(),
    actor       text NOT NULL,
    action      text NOT NULL,
    target_type text,
    target_id   text,
    detail      jsonb,
    -- Which tenant the entry belongs to. An organisation administrator must see
    -- everything done inside their own tenant and nothing outside it. Null
    -- means the action was not scoped to one - a change to the shared model
    -- catalogue, say - and only an operator sees those.
    org_id      text
);
CREATE INDEX audit_log_ts_idx ON audit_log (ts);
CREATE INDEX audit_log_org_id_idx ON audit_log (org_id, id DESC);
-- A compliance reader filters by actor and by action before they read anything
-- at all.
CREATE INDEX audit_log_actor_idx ON audit_log (actor, id DESC);
CREATE INDEX audit_log_action_idx ON audit_log (action, id DESC);

-- ---------------------------------------------------------------------------
-- Signing in
-- ---------------------------------------------------------------------------

CREATE TABLE sessions (
    -- SHA-256 of the cookie value, for the same reason api_keys stores a hash:
    -- a database dump must not be a set of working credentials.
    id           bytea PRIMARY KEY,
    user_id      text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Double-submit token. SameSite=Lax already blocks the cross-site form
    -- post; this covers the rest.
    csrf         text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    user_agent   text NOT NULL DEFAULT '',
    ip           text NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- One row per login in flight, holding the PKCE verifier and the nonce. It
-- lives in the database rather than in memory so that the redirect and the
-- callback may land on different replicas.
CREATE TABLE login_flows (
    state       text PRIMARY KEY,
    verifier    text NOT NULL,
    nonce       text NOT NULL,
    redirect_to text NOT NULL DEFAULT '',
    -- Which identity provider this login belongs to. The callback needs it to
    -- know which client to complete the exchange with, and it comes from the
    -- flow rather than from the callback URL so that every provider can share
    -- one redirect URI.
    provider    text NOT NULL DEFAULT '',
    -- Where to send the browser once the identity provider is done with it, and
    -- what the process waiting there has to prove to redeem the code. They live
    -- on the flow rather than in the callback URL for the reason the provider
    -- does: the callback is a URL the identity provider redirects to, so
    -- anything read out of it there is a parameter a stranger can write.
    cli_redirect  text NOT NULL DEFAULT '',
    cli_challenge text NOT NULL DEFAULT '',
    cli_state     text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);
CREATE INDEX login_flows_expires_at_idx ON login_flows (expires_at);

-- The one-time code the browser carries to the loopback listener.
--
-- Signing in from the command line is the flow RFC 8252 describes for a native
-- application: the CLI listens on a loopback port, the browser does the
-- ordinary sign-in against the deployment's identity provider, and the gateway
-- hands the result back through that port.
--
-- This code exists because the thing that completes a sign-in is a redirect to
-- a browser, and what needs the credential is a process the browser cannot hand
-- anything to except through a URL - which lands in shell history, in the
-- browser's own history, and in any extension that reads it. So the URL carries
-- a code that is worth nothing without the PKCE verifier the CLI kept to
-- itself, lives for minutes, and is deleted the first time it is redeemed.
CREATE TABLE cli_codes (
    -- SHA-256 of the code, for the same reason sessions and api_keys store a
    -- hash: a database dump must not be a set of working credentials.
    id         bytea PRIMARY KEY,
    user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The S256 challenge from the request that started this sign-in. Redeeming
    -- the code means producing the verifier behind it, which is what stops
    -- another process on the same machine from racing to the loopback port.
    challenge  text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX cli_codes_expires_at_idx ON cli_codes (expires_at);

-- What a signed-in command line holds.
--
-- It is a session by another name, and deliberately not the sessions table: a
-- session is carried by a browser, which means a cookie, a CSRF token and
-- twelve hours. This is carried in a file by a person who will be typing
-- commands next week, presented in an Authorization header where cross-site
-- request forgery is not a thing that exists, and it says which machine it is
-- on so that one laptop can be signed out without the others.
CREATE TABLE cli_tokens (
    id           bytea PRIMARY KEY,
    user_id      text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The machine it was issued to, as that machine described itself. Shown to
    -- a person deciding which of their sign-ins is which; nothing reads it to
    -- decide anything.
    label        text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
CREATE INDEX cli_tokens_user_id_idx ON cli_tokens (user_id);
CREATE INDEX cli_tokens_expires_at_idx ON cli_tokens (expires_at);

-- ---------------------------------------------------------------------------
-- Sandboxes: the machine the gateway lends out
-- ---------------------------------------------------------------------------

-- Everything above is about the request an editor sends. None of it governs
-- what the agent does with the answer, because the agent runs on a laptop this
-- gateway cannot see. A sandbox is that laptop moved inside the cluster - which
-- is what makes three things possible that were not: an egress rule an agent
-- cannot talk its way past, an API key that never lands on anybody's disk, and
-- a session that is stated rather than inferred.
--
-- Two tables. The first is a catalogue and behaves exactly like models: it is
-- declared in a file, applied on every start, and owned by the deployment. The
-- second is the sandboxes themselves, and it is a log rather than a live view -
-- rows are kept after the sandbox is gone, because what a task cost and who
-- asked for it outlive the machine that answered.

-- The catalogue: a machine somebody can ask for by name.
--
-- The name is a contract in the way a model alias is. It is typed on a command
-- line, written into a repository's own configuration and baked into a team's
-- habits, so the operator has to be able to change the image behind it, move it
-- to a stronger isolation tier or give it more memory without anybody editing
-- anything - and everything below exists to protect that.
CREATE TABLE sandbox_classes (
    name         text PRIMARY KEY,
    description  text NOT NULL DEFAULT '',
    -- What the sandbox runs. It should carry the toolchain already installed:
    -- a sandbox that installs its own is one nobody waits for, and on an
    -- air-gapped site it is one that never becomes ready at all.
    image        text NOT NULL,
    -- How firmly this class is separated from the node: standard, isolated or
    -- vm. Stated as intent rather than as a runtime name, because what a
    -- cluster calls its gVisor RuntimeClass is that cluster's business - the
    -- deployment maps these three onto its own names once, and the catalogue
    -- stays portable between clusters.
    isolation    text NOT NULL DEFAULT 'isolated',
    -- The escape hatch from that mapping, for a cluster with two of something
    -- or a hand-rolled runtime. An entry that sets it has stopped being
    -- portable, which is why it is not the ordinary way to answer the question.
    runtime_class text NOT NULL DEFAULT '',
    cpu_millis   integer NOT NULL DEFAULT 2000,
    memory_mib   integer NOT NULL DEFAULT 4096,
    -- Zero is a real answer: a class with no volume keeps nothing across a
    -- suspend, which is right for an agent sandbox that exists for one task
    -- and pushes a branch at the end of it.
    disk_mib     integer NOT NULL DEFAULT 0,
    -- Seconds rather than an interval, because this number crosses JSON, a
    -- form field and a command-line flag as well as SQL, and a duration that
    -- has to parse the same way in four places is one that eventually does not.
    --
    -- default_ttl is what a sandbox of this class gets when nobody says;
    -- max_ttl is the ceiling on what anybody may ask for, before a guardrail
    -- narrows it further. Neither may be absent: a sandbox with no expiry is a
    -- virtual machine somebody has to remember to delete, and every platform
    -- that has offered one has ended up with a cluster full of them.
    default_ttl_seconds integer NOT NULL DEFAULT 14400,
    max_ttl_seconds     integer NOT NULL DEFAULT 86400,
    -- How many of this class are kept started and idle so that asking for one
    -- is instant. It is the difference between a feature people use and one
    -- they work around, and it is also a standing bill: this many of the CPU
    -- and memory above, held by nobody, all the time.
    warm         integer NOT NULL DEFAULT 0,
    -- Where a sandbox of this class may open a connection, by the names the
    -- deployment's own network policy knows. An empty list is not "anywhere" -
    -- it is the driver's default, which is the gateway and DNS and nothing
    -- else, because a sandbox that can reach the internet is a sandbox a
    -- repository can leave from.
    egress       text[] NOT NULL DEFAULT '{}',
    -- Which of engineer and agent may ask for this class. Empty is both.
    purposes     text[] NOT NULL DEFAULT '{}',
    -- Declared by the catalogue file, which is applied on every start. Nothing
    -- but another apply may change such a row: an edit made elsewhere would be
    -- silently undone by the next restart.
    managed      boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- The sandboxes themselves.
CREATE TABLE sandboxes (
    id        text PRIMARY KEY,
    org_id    text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    -- The team and the person are kept even when the row they pointed at goes.
    -- A sandbox is a thing that cost money and held source code, so the
    -- question "whose was this" has to keep an answer after somebody leaves -
    -- which is why these are SET NULL rather than CASCADE, and why the
    -- addresses beside them are copied rather than joined.
    team_id   text REFERENCES teams (id) ON DELETE SET NULL,
    user_id   text REFERENCES users (id) ON DELETE SET NULL,
    owner     text NOT NULL DEFAULT '',
    -- What the developer called it. It is a DNS label inside the cluster and
    -- half of an ssh config Host pattern on their laptop, which is why it is
    -- held to the same shape as a model alias.
    name      text NOT NULL,
    -- Deliberately not a foreign key into sandbox_classes, for the reason
    -- filters.model is not one into models: the catalogue is shared and a
    -- sandbox is not, so one team's running sandbox must not make a class
    -- undeletable. Everything the sandbox actually needs from the class is
    -- copied below.
    class     text NOT NULL,
    purpose   text NOT NULL DEFAULT 'engineer',
    state     text NOT NULL DEFAULT 'pending',
    -- Why, for a state that is not ready. It carries the scheduler's own words
    -- where there are any: "0/6 nodes are available: insufficient memory" is
    -- the answer to "why is my sandbox still starting", and anything this
    -- gateway wrote instead would be a worse version of it.
    detail    text NOT NULL DEFAULT '',

    -- The machine as it actually was, copied from the class at creation rather
    -- than joined at read time.
    --
    -- The class is allowed to change underneath a running sandbox - that is
    -- the whole point of the name being a contract - so a bill or an incident
    -- report that joined would describe a machine that never ran. These four
    -- columns are what the sandbox-seconds below are multiplied by.
    image      text NOT NULL DEFAULT '',
    isolation  text NOT NULL DEFAULT '',
    cpu_millis integer NOT NULL DEFAULT 0,
    memory_mib integer NOT NULL DEFAULT 0,
    disk_mib   integer NOT NULL DEFAULT 0,

    -- The key minted for this sandbox, which is the whole of how the agent
    -- inside it reaches the gateway. It is created with the sandbox, expires
    -- with it and is revoked when it goes - so the credential never exists on
    -- anybody's laptop, and a sandbox that is terminated takes its access with
    -- it.
    key_id    text REFERENCES api_keys (id) ON DELETE SET NULL,
    -- The forge's id for the repository credential this sandbox holds, so it
    -- can be revoked when the sandbox ends. Empty when the forge's credentials
    -- cannot be revoked and only expire, as a GitHub App token does after an
    -- hour. The credential itself is never stored.
    git_credential_id text NOT NULL DEFAULT '',
    -- The session an agent sandbox's requests belong to, precomputed from its
    -- key and its id exactly as the gateway would compute it from the header
    -- the sandbox sends.
    --
    -- docs/sessions.md records that session grouping is an inference and lists
    -- the two ways it is wrong. An agent sandbox is one task by construction,
    -- so it states its own id instead - and this column is what lets a sandbox
    -- and the task it carried out be read next to each other, which a hash
    -- alone could never support.
    session_key text,

    -- Where the work came from and where it went. The branch is the only thing
    -- that ever leaves an agent sandbox, which is what makes the egress rule
    -- around it tight enough to be worth writing.
    repo      text NOT NULL DEFAULT '',
    branch    text NOT NULL DEFAULT '',

    node      text NOT NULL DEFAULT '',
    address   text NOT NULL DEFAULT '',

    -- Which kind of object in the cluster this sandbox is: one the gateway
    -- created, or a claim on a warm pool.
    --
    -- Stored rather than derived from configuration, because the two are
    -- addressed differently and a deployment that turns warm pools off still
    -- has the sandboxes it handed out while they were on. Working out how to
    -- reach one from the current settings would lose every one of them at
    -- exactly that moment.
    backing   text NOT NULL DEFAULT 'sandbox',

    created_at   timestamptz NOT NULL DEFAULT now(),
    ready_at     timestamptz,
    expires_at   timestamptz,
    suspended_at timestamptz,
    -- Terminated rather than deleted: everything else this schema deletes is a
    -- record that stops existing, and a terminated sandbox is the opposite -
    -- the machine is gone and the row is deliberately kept, because what it
    -- cost and who it belonged to outlive it.
    terminated_at timestamptz,

    -- How long this sandbox has actually held compute, accumulated.
    --
    -- It is what puts a sandbox on the same footing as a token: a task that
    -- cost one franc twenty in tokens and eleven minutes of a four-core machine
    -- is a number a budget conversation can use, and the first half on its own
    -- is not. It is accumulated rather than derived from the timestamps because
    -- a sandbox can be suspended and resumed any number of times, and summing
    -- those intervals at read time would need a second table of them.
    running_seconds bigint NOT NULL DEFAULT 0,
    -- The instant running_seconds was last brought up to date. The sweep adds
    -- the elapsed time and moves this forward, so a gateway that was down
    -- charges for the time it could not observe - which is correct: the sandbox
    -- was running.
    accounted_at timestamptz
);

-- A name is unique among the sandboxes an organisation currently has, and free
-- again once one is gone.
--
-- Partial, on the live states only, and that is the point of it. `keera sandbox
-- ssh fix-login` names a sandbox by name, so two live ones sharing a name would
-- be ambiguous at the moment it matters most; but a developer who finishes with
-- "fix-login" on Tuesday and wants it again on Thursday is asking for something
-- perfectly reasonable, and a unique index over every row this table has ever
-- held would refuse it for ever.
CREATE UNIQUE INDEX sandboxes_live_name_idx ON sandboxes (org_id, name)
    WHERE state IN ('pending', 'ready', 'suspended');

-- The list, which is always one organisation's, newest first.
CREATE INDEX sandboxes_org_idx ON sandboxes (org_id, created_at DESC);

-- What the sweep reads: the sandboxes whose time is up. Partial on the live
-- states, because this runs every minute and the table is mostly history.
CREATE INDEX sandboxes_expiry_idx ON sandboxes (expires_at)
    WHERE state IN ('pending', 'ready', 'suspended');

-- A person's own sandboxes, for the developer's screen.
CREATE INDEX sandboxes_user_idx ON sandboxes (user_id, created_at DESC)
    WHERE user_id IS NOT NULL;
