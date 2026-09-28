// One team, one key, one model - the dashboard narrowed to it, and the
// requests or sessions behind the numbers.
//
// The list screens answer "what exists and what is it configured to do". They
// cannot answer the question somebody arrives with, which is always about one
// thing: this team's spend jumped on Tuesday, this key stopped working this
// afternoon, this model has gone slow. That is the dashboard's eight numbers
// over a narrower set of rows, plus the rows themselves - because a chart says
// when something changed and only the log says what changed.
//
// All three are the same screen with a different scope, so whoever learns to
// read one has learned to read the other two and the totals cannot drift
// apart.

import { api } from "../api.js";
import {
  h,
  stat,
  pill,
  meter,
  empty,
  confirm,
  toast,
  go,
  compact,
  num,
  money,
  ms,
  ago,
  date,
  dateTime,
  icon,
  icons,
  currentRange,
  rangePicker,
  locationName,
  releaseDay,
  crumb,
  isAdmin,
} from "../ui.js";
import { areaChart, barList } from "../chart.js";
import { requestLog, setOutcome } from "./requestlog.js";
import { sessionLog, setUnhappy } from "./sessions.js";
import { modelsCell, summarise } from "./guardrails.js";
import { canEditModels, openModel } from "./models.js";
import { canRevoke, openKey, revokeBody, stateOf } from "./keys.js";
import { openTeam } from "./teams.js";
import { chooseOrg, orgNameOf } from "./orgs.js";

/* ------------------------------------------------------------------ teams */

export async function teamDetailView(ctx) {
  const teamsRes = await api.teams(ctx.orgID);
  const team = (teamsRes.data || []).find((t) => t.id === ctx.param);
  // The team's own organisation, not the switcher's: with "All
  // organisations" chosen there is no org to list models for.
  const modelsRes = team
    ? await api.models(team.org_id).catch(() => ({ data: [] }))
    : { data: [] };
  const models = (modelsRes.data || []).filter((m) => m.enabled !== false);
  const canEdit = isAdmin(ctx);

  if (!team) {
    return gone(
      ctx,
      "/teams",
      "Teams",
      "No such team",
      "It was deleted, or it belongs to another organisation. Its requests " +
        "stay in the usage report.",
    );
  }

  const orgID = team.org_id || ctx.orgID || ctx.state.me.org_id;
  const orgName = orgNameOf(ctx, orgID);

  return screen(ctx, {
    back: { path: "/teams", label: "Teams" },
    title: team.name,
    identity: team.id,
    scope: { team_id: team.id },
    hide: { team: true },
    meta: [
      pill(
        `${num(team.active_keys)} key${team.active_keys === 1 ? "" : "s"} not revoked`,
      ),
      budgetPill(ctx, team),
    ],
    actions: [
      h(
        "button",
        {
          class: "btn",
          title: "Guardrails for this team's keys",
          onClick: () => openTeam(ctx, team, models, orgID, orgName, canEdit),
        },
        icon(icons.sliders),
        canEdit ? "Guardrails" : "View guardrails",
      ),
    ],
    panels: (o, names) => [
      barPanel(
        "Models by tokens",
        o.top_models,
        (r) => r.group || "unknown model",
        (r) => r.input_tokens + r.output_tokens,
        1,
      ),
      barPanel(
        "Keys by spend",
        o.top_keys,
        (r) => names.keys[r.group] || r.group || "No key",
        (r) => r.cost_micros || r.requests,
        3,
        ctx.currency,
      ),
    ],
    aside: h(
      "div",
      { class: "card" },
      h("div", { class: "card-head" }, h("h2", {}, "What this team allows")),
      h(
        "div",
        { class: "card-body" },
        facts([
          ["Models", modelsCell(team.limits)],
          [
            "Guardrails",
            summarise(team.limits).length
              ? h(
                  "span",
                  { class: "muted" },
                  summarise(team.limits).join(" · "),
                )
              : h(
                  "span",
                  { class: "faint" },
                  "inherited from the organisation",
                ),
          ],
          ["Created", h("span", { class: "muted" }, date(team.created_at))],
        ]),
      ),
    ),
    note:
      "This team's guardrails apply to every key in it. A team can only " +
      "narrow what the organisation allows. Refused requests show below and " +
      "cost nothing.",
  });
}

/* ------------------------------------------------------------------- keys */

export async function keyDetailView(ctx) {
  const [keysRes, teamsRes, usersRes] = await Promise.all([
    api.keys(ctx.orgID),
    api.teams(ctx.orgID).catch(() => ({ data: [] })),
    api.users(ctx.orgID).catch(() => ({ data: [] })),
  ]);
  const key = (keysRes.data || []).find((k) => k.id === ctx.param);
  // The key's own organisation, for the same reason as on a team's page.
  const modelsRes = key
    ? await api.models(key.org_id).catch(() => ({ data: [] }))
    : { data: [] };
  const models = (modelsRes.data || []).filter((m) => m.enabled !== false);
  const canEdit = isAdmin(ctx);

  if (!key) {
    return gone(
      ctx,
      "/keys",
      "API keys",
      "No such key",
      "No key has this id. Revoked keys stay listed, so it was not revoked.",
    );
  }

  const teamName = (teamsRes.data || []).find((t) => t.id === key.team_id);
  const person = (usersRes.data || []).find((u) => u.id === key.user_id);
  const state = stateOf(key);

  return screen(ctx, {
    back: { path: "/keys", label: "API keys" },
    title: key.alias,
    identity: key.prefix + "…",
    scope: { key_id: key.id },
    // A key is one person or one pipeline, so its traffic reads best as the
    // tasks it ran rather than as single requests.
    sessions: true,
    hide: { who: true },
    meta: [
      state === "revoked"
        ? pill("Revoked", "bad")
        : state === "expired"
          ? pill("Expired", "warn")
          : pill("Active", "good"),
      key.team_id
        ? h(
            "a",
            {
              class: "pill pill-button",
              href: "/teams/" + encodeURIComponent(key.team_id),
              title: "Open this key's team",
              onClick: go(ctx, "/teams/" + encodeURIComponent(key.team_id)),
            },
            teamName ? teamName.name : key.team_id,
          )
        : pill("Organisation-wide"),
      person ? pill(person.email) : null,
    ],
    actions: [
      h(
        "button",
        {
          class: "btn",
          title: "Guardrails for this key",
          onClick: () =>
            openKey(ctx, key, models, teamName ? teamName.name : "", canEdit),
        },
        icon(icons.sliders),
        canEdit ? "Guardrails" : "View guardrails",
      ),
      canRevoke(ctx, key) && state === "active"
        ? h(
            "button",
            {
              class: "btn btn-danger",
              onClick: () =>
                confirm({
                  title: "Revoke this key?",
                  body: revokeBody(key),
                  confirmLabel: "Revoke",
                  danger: true,
                  onConfirm: async () => {
                    await api.revokeKey(key.id);
                    toast("Key revoked", "good");
                    ctx.navigate("/keys");
                  },
                }),
            },
            "Revoke",
          )
        : null,
    ],
    panels: (o) => [
      barPanel(
        "Models by tokens",
        o.top_models,
        (r) => r.group || "unknown model",
        (r) => r.input_tokens + r.output_tokens,
        1,
      ),
      h(
        "div",
        { class: "card" },
        h("div", { class: "card-head" }, h("h2", {}, "This key")),
        h(
          "div",
          { class: "card-body" },
          facts([
            ["Prefix", h("span", { class: "mono muted" }, key.prefix + "…")],
            ["Issued", h("span", { class: "muted" }, date(key.created_at))],
            [
              "Expires",
              key.expires_at
                ? h("span", { class: "muted" }, date(key.expires_at))
                : h("span", { class: "faint" }, "never"),
            ],
            [
              "Last used",
              key.last_used_at
                ? h(
                    "span",
                    { class: "muted", title: dateTime(key.last_used_at) },
                    ago(key.last_used_at),
                  )
                : h("span", { class: "faint" }, "not this month"),
            ],
            [
              "Guardrails",
              summarise(key.limits || {}).length
                ? h(
                    "span",
                    { class: "muted" },
                    summarise(key.limits).join(" · "),
                  )
                : h("span", { class: "faint" }, "the team's"),
            ],
          ]),
        ),
      ),
    ],
    note:
      "Last used counts every request with this key, including ones a " +
      "guardrail refused.",
  });
}

/* ----------------------------------------------------------------- models */

export async function modelDetailView(ctx) {
  if (!ctx.orgID) return chooseOrg(ctx, "Models");
  const models = (await api.models(ctx.orgID)).data || [];
  const model = models.find((m) => m.alias === ctx.param);
  const canEdit = model ? canEditModels(ctx) : false;

  return screen(ctx, {
    back: { path: "/models", label: "Models" },
    title: ctx.param,
    mono: true,
    identity: model
      ? "serves " + model.backend_model
      : `not one of ${orgNameOf(ctx)}'s models`,
    scope: { alias: ctx.param },
    hide: { model: true },
    meta: model
      ? [
          pill(model.kind),
          model.enabled ? pill("Enabled", "good") : pill("Disabled", "warn"),
          model.max_context
            ? pill(`${compact(model.max_context)} context`)
            : null,
          model.location ? pill(locationName(model.location)) : null,
          model.release_date
            ? pill(`released ${releaseDay(model.release_date)}`)
            : null,
        ]
      : [pill("Removed", "warn")],
    // A model that was removed still has traffic behind it, and that traffic is
    // usually why somebody is here. The screen draws it and says why the
    // catalogue has nothing to show next to it, rather than refusing to open.
    banner: model
      ? null
      : `This alias is no longer one of ${orgNameOf(ctx)}'s models. Requests for it are ` +
        "refused with a 404. Its history is below.",
    actions: model
      ? [
          h(
            "button",
            {
              class: "btn",
              title: canEdit
                ? "Edit this model's settings"
                : "This model's settings",
              onClick: () => openModel(ctx, model),
            },
            icon(icons.models),
            canEdit ? "Edit model" : "View model",
          ),
        ]
      : [],
    panels: (o, names) => [
      barPanel(
        "Teams by spend",
        o.top_teams,
        (r) => names.teams[r.group] || r.group || "No team",
        (r) => r.cost_micros || r.requests,
        0,
        ctx.currency,
      ),
      barPanel(
        "Keys by spend",
        o.top_keys,
        (r) => names.keys[r.group] || r.group || "No key",
        (r) => r.cost_micros || r.requests,
        3,
        ctx.currency,
      ),
    ],
    aside: model
      ? h(
          "div",
          { class: "card" },
          h(
            "div",
            { class: "card-head" },
            h("h2", {}, "What it is configured to do"),
          ),
          h(
            "div",
            { class: "card-body" },
            facts([
              [
                "Serves",
                h("span", { class: "mono muted" }, model.backend_model),
              ],
              [
                "Backends",
                (model.backends || []).length
                  ? h(
                      "div",
                      { class: "stack" },
                      (model.backends || []).map((b) =>
                        h(
                          "span",
                          {
                            class: "mono muted",
                            style: { fontSize: "11.5px" },
                          },
                          b,
                        ),
                      ),
                    )
                  : h(
                      "span",
                      { class: "faint" },
                      // Backends are for administrators, so an empty list
                      // does not mean there are none.
                      canEditModels(ctx) ? "none" : "not shown to your role",
                    ),
              ],
              [
                `Price / Mtok (${ctx.currency})`,
                model.input_micros_per_mtok || model.output_micros_per_mtok
                  ? h(
                      "span",
                      { class: "muted nowrap" },
                      `${money(model.input_micros_per_mtok, "")} in · ` +
                        `${money(model.output_micros_per_mtok, "")} out`,
                    )
                  : h("span", { class: "faint" }, "not billed"),
              ],
            ]),
          ),
        )
      : null,
    note:
      "A failure is a 5xx: the backend did not answer, or no backend, filter " +
      "or router destination was available. Each failed row shows the " +
      "message.",
  });
}

/* ----------------------------------------------------------- the scaffold */

// screen draws all three. Everything above the chart is what makes this entity
// that entity; everything from the chart down is the same report every time.
async function screen(ctx, spec) {
  const since = currentRange();
  const res = await api.overview(ctx.orgID, since, spec.scope);
  const o = res.overview;
  const currency = res.currency || ctx.currency;
  const names = { teams: res.team_names || {}, keys: res.key_aliases || {} };

  ctx.setTitle(spec.title, spec.identity || "");

  const head = h(
    "div",
    { class: "detail-head" },
    crumb(ctx, spec.back.path, spec.back.label),
    h(
      "div",
      { class: "row", style: { flexWrap: "wrap" } },
      h("div", { class: "wrap-chips" }, spec.meta.filter(Boolean)),
      h("div", { style: { flex: 1 } }),
      rangePicker(ctx, since),
      spec.actions.filter(Boolean),
    ),
  );

  const wrap = h("div", {}, head);
  if (spec.banner) {
    wrap.append(
      h(
        "div",
        { class: "banner banner-warn", style: { marginBottom: "16px" } },
        spec.banner,
      ),
    );
  }

  const failRate = o.requests ? (o.failed / o.requests) * 100 : 0;
  wrap.append(
    h(
      "div",
      { class: "grid grid-4" },
      stat(
        "Requests",
        compact(o.requests),
        null,
        o.refused ? `${num(o.refused)} refused` : "none refused",
      ),
      stat(
        "Tokens",
        compact(o.input_tokens + o.output_tokens),
        null,
        `${compact(o.input_tokens)} in · ${compact(o.output_tokens)} out`,
      ),
      stat("Spend", money(o.cost_micros, ""), currency),
      stat(
        "First token",
        ms(o.ttft_median_ms),
        null,
        `p95 ${ms(o.ttft_p95_ms)}`,
      ),
    ),
  );

  // The chart. Its failure count is a way in to the rows rather than a
  // decoration: on this screen those rows are already below it, so the count
  // narrows the log in place instead of sending anybody to another screen and
  // asking them to reconstruct the filter they arrived with.
  wrap.append(
    h(
      "div",
      { style: { marginTop: "16px" } },
      h(
        "div",
        { class: "card" },
        h(
          "div",
          { class: "card-head" },
          h("h2", {}, "Traffic"),
          h("div", { class: "spacer" }),
          o.failed > 0
            ? failureJump(
                ctx,
                `${num(o.failed)} failed (${failRate.toFixed(1)}%)`,
                spec.sessions,
              )
            : h(
                "span",
                { class: "pill pill-good" },
                h("span", { class: "dot" }),
                "no failures",
              ),
        ),
        h(
          "div",
          { class: "card-body" },
          o.requests === 0
            ? empty(
                "Nothing recorded in this window",
                spec.sessions
                  ? "Widen the range."
                  : "Widen the range, or see the refused requests below.",
              )
            : areaChart(o.series, { currency, bucket: o.bucket }),
        ),
      ),
    ),
  );

  const panels = spec.panels(o, names).filter(Boolean);
  if (spec.aside) panels.push(spec.aside);
  if (panels.length) {
    wrap.append(
      h(
        "div",
        {
          class: panels.length === 3 ? "grid grid-3" : "grid grid-2",
          style: { marginTop: "16px" },
        },
        panels,
      ),
    );
  }

  // The log is administrator-only: its rows name other people's keys and carry
  // text the inference plane wrote. A member gets the charts, which are their own organisation's
  // numbers and nobody's individual traffic.
  if (isAdmin(ctx)) {
    const log = spec.sessions ? sessionLog : requestLog;
    wrap.append(
      await log(ctx, { scope: spec.scope, since, hide: spec.hide || {} }),
    );
  }

  if (spec.note)
    wrap.append(
      h("div", { class: "hint", style: { marginTop: "16px" } }, spec.note),
    );
  return wrap;
}

// failureJump is the failed count as the way to the failed rows, which on this
// screen are a scroll away rather than a page away. Under a session log it
// shows the tasks that hit trouble.
function failureJump(ctx, text, sessions) {
  if (!isAdmin(ctx)) {
    return h(
      "span",
      { class: "pill pill-bad" },
      h("span", { class: "dot" }),
      text,
    );
  }
  return h(
    "button",
    {
      class: "pill pill-bad pill-button",
      title: sessions
        ? "Show only the tasks that hit trouble below"
        : "Show only the failed requests below",
      onClick: () => {
        if (sessions) setUnhappy(true);
        else setOutcome("failed");
        ctx.reload();
      },
    },
    h("span", { class: "dot" }),
    text,
  );
}

function barPanel(title, rows, label, value, colorIndex, currency) {
  return h(
    "div",
    { class: "card" },
    h("div", { class: "card-head" }, h("h2", {}, title)),
    h(
      "div",
      { class: "card-body" },
      barList(rows || [], { label, value, colorIndex, currency }),
    ),
  );
}

export function facts(rows) {
  return h(
    "div",
    { class: "facts" },
    rows
      .filter(Boolean)
      .map(([k, v]) =>
        h(
          "div",
          { class: "fact" },
          h("div", { class: "stat-label" }, k),
          h("div", {}, v),
        ),
      ),
  );
}

/* ------------------------------------------------------------------ bits */

function budgetPill(ctx, team) {
  if (!team.budget_micros) {
    return pill(
      `${money(team.spend_micros, ctx.currency)} this month, no budget`,
    );
  }
  const frac = team.spend_micros / team.budget_micros;
  const tone = frac >= 1 ? "bad" : frac >= 0.8 ? "warn" : "";
  return h(
    "span",
    { class: "pill" },
    meter(frac, tone),
    `${money(team.spend_micros, "")} of ${money(team.budget_micros, ctx.currency)} per ${team.period}`,
  );
}

// gone is the screen for an id that names nothing. It is an answer rather than
// an error: the usual way to reach one is a bookmark from before the thing was
// deleted, and "it is not there any more" is what the reader needs to be told.
function gone(ctx, path, label, title, body) {
  return h(
    "div",
    {},
    crumb(ctx, path, label),
    h("div", { class: "card" }, empty(title, body)),
  );
}
