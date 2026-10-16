// One project, one key, one model, one user - the dashboard narrowed to it,
// and the requests or sessions behind the numbers.
//
// The list screens answer "what exists and what is it configured to do". They
// cannot answer the question somebody arrives with, which is always about one
// thing: this project's spend jumped on Tuesday, this key stopped working this
// afternoon, this model has gone slow. That is the dashboard's eight numbers
// over a narrower set of rows, plus the rows themselves - because a chart says
// when something changed and only the log says what changed.
//
// All four are the same screen with a different scope, so whoever learns to
// read one has learned to read the others and the totals cannot drift
// apart.

import { api } from "../api.js";
import {
  h,
  stat,
  pill,
  meter,
  empty,
  go,
  compact,
  num,
  listPrice,
  money,
  moneyStat,
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
  gone,
  isAdmin,
  plural,
  pct,
  detailHead,
  busyButton,
  banner,
} from "../ui.js";
import { areaChart, barList } from "../chart.js";
import { requestLog } from "./requestlog.js";
import { setOutcome, namesFrom } from "./logfilters.js";
import { sessionLog } from "./sessions.js";
import { modelsCell, summarise } from "./guardrails.js";
import { openModel, checkModel, servedName } from "./models.js";
import {
  activateButton,
  canActivate,
  canRevoke,
  canRotate,
  confirmRevoke,
  confirmRotate,
  renameKey,
  inPeriod,
  isLive,
  NO_USER,
  openKey,
  prefixOf,
  statePill,
  stateOf,
} from "./keys.js";
import { openProject, editProject, deleteProject } from "./projects.js";
import { orgNameOf } from "./orgs.js";
import {
  TONE,
  roleLabel,
  signsInWith,
  changeRole,
  disablePerson,
  enablePerson,
} from "./people.js";

/* ------------------------------------------------------------------ projects */

export async function projectDetailView(ctx) {
  const projectsRes = await api.projects(ctx.orgID);
  const project = (projectsRes.data || []).find((t) => t.id === ctx.param);
  // The project's own organisation, not the switcher's: with "All
  // organisations" chosen there is no org to list models for.
  const modelsRes = project
    ? await api.models(project.org_id).catch(() => ({ data: [] }))
    : { data: [] };
  const models = (modelsRes.data || []).filter((m) => m.enabled !== false);
  const canEdit = isAdmin(ctx);

  if (!project) {
    return gone(
      ctx,
      "/projects",
      "Projects",
      "No such project",
      "It was deleted, or it belongs to another organisation. Its requests " +
        "stay in the usage report.",
    );
  }

  const orgID = project.org_id || ctx.orgID || ctx.state.me.org_id;
  const orgName = orgNameOf(ctx, orgID);

  return screen(ctx, {
    back: { path: "/projects", label: "Projects" },
    title: project.name,
    identity: project.id,
    description: project.description,
    scope: { project_id: project.id },
    hide: { project: true },
    meta: [
      pill(
        `${num(project.active_keys)} ${plural(project.active_keys, "key")} not revoked`,
      ),
      budgetPill(ctx, project),
    ],
    actions: [
      h(
        "button",
        {
          class: "btn",
          title: "Guardrails for this project's keys",
          onClick: () =>
            openProject(ctx, project, models, orgID, orgName, canEdit),
        },
        icon(icons.sliders),
        canEdit ? "Guardrails" : "View guardrails",
      ),
      canEdit
        ? h(
            "button",
            {
              class: "btn",
              title: "Edit this project",
              onClick: () => editProject(ctx, project),
            },
            icon(icons.pencil),
            "Edit",
          )
        : null,
      canEdit
        ? h(
            "button",
            {
              class: "btn btn-danger",
              title: "Delete this project",
              onClick: () =>
                deleteProject(ctx, project, () => ctx.navigate("/projects")),
            },
            icon(icons.trash),
            "Delete",
          )
        : null,
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
        "Keys by cost",
        o.top_keys,
        (r) => names.keys[r.group] || r.group || "No key",
        (r) => r.cost_micros,
        3,
        true,
      ),
    ],
    aside: h(
      "div",
      { class: "card" },
      h("div", { class: "card-head" }, h("h2", {}, "What this project allows")),
      h(
        "div",
        { class: "card-body" },
        facts([
          ["Models", modelsCell(project.limits)],
          [
            "Guardrails",
            summarise(project.limits).length
              ? h(
                  "span",
                  { class: "muted" },
                  summarise(project.limits).join(" · "),
                )
              : h("span", { class: "faint" }, `Inherited from ${orgName}`),
          ],
          ["Created", h("span", { class: "muted" }, date(project.created_at))],
        ]),
      ),
    ),
    note:
      "This project's guardrails apply to every key in it. A project can only " +
      "narrow what the organisation allows. Refused requests show below and " +
      "cost nothing.",
  });
}

/* ------------------------------------------------------------------- keys */

export async function keyDetailView(ctx) {
  // Read over the picker's period, so Last used covers the same span as the
  // numbers below it.
  const since = currentRange();
  const [keysRes, projectsRes, usersRes] = await Promise.all([
    api.keys(ctx.orgID, { since }),
    api.projects(ctx.orgID).catch(() => ({ data: [] })),
    api.users(ctx.orgID).catch(() => ({ data: [] })),
  ]);
  const key = (keysRes.data || []).find((k) => k.id === ctx.param);
  // The key's own organisation, for the same reason as on a project's page.
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

  const project = (projectsRes.data || []).find((t) => t.id === key.project_id);
  const person = (usersRes.data || []).find((u) => u.id === key.user_id);
  const state = stateOf(key);

  return screen(ctx, {
    back: { path: "/keys", label: "API keys" },
    title: key.name,
    identity: prefixOf(key),
    scope: { key_id: key.id },
    // A key is one person or one pipeline, so its traffic reads best as the
    // tasks it ran rather than as single requests.
    sessions: true,
    hide: { who: true },
    meta: [
      statePill(state),
      // Only a revoked key can have none: its project was deleted.
      key.project_id
        ? h(
            "a",
            {
              class: "pill pill-button",
              href: "/projects/" + encodeURIComponent(key.project_id),
              title: "Open this key's project",
              onClick: go(
                ctx,
                "/projects/" + encodeURIComponent(key.project_id),
              ),
            },
            project ? project.name : key.project_id,
          )
        : pill("Deleted project"),
      // Only an administrator has the Users screen to open.
      person && isAdmin(ctx)
        ? h(
            "a",
            {
              class: "pill pill-button",
              href: "/users/" + encodeURIComponent(person.id),
              title: "Open this key's user",
              onClick: go(ctx, "/users/" + encodeURIComponent(person.id)),
            },
            person.email,
          )
        : person
          ? pill(person.email)
          : null,
    ],
    actions: [
      h(
        "button",
        {
          class: "btn",
          title: "Guardrails for this key",
          onClick: () =>
            openKey(ctx, key, models, project ? project.name : "", canEdit),
        },
        icon(icons.sliders),
        canEdit ? "Guardrails" : "View guardrails",
      ),
      canRevoke(ctx, key)
        ? h(
            "button",
            {
              class: "btn",
              title: "Rename this key",
              onClick: () => renameKey(ctx, key),
            },
            icon(icons.pencil),
            "Rename",
          )
        : null,
      state === "pending" && canActivate(ctx, key)
        ? activateButton(ctx, key)
        : null,
      // The old key is revoked by the rotation, so the screen moves to the
      // new one.
      canRevoke(ctx, key) && canRotate(key)
        ? h(
            "button",
            {
              class: "btn",
              title: "Replace this key with a new one",
              onClick: () =>
                confirmRotate(ctx, key, {
                  done: (created) =>
                    ctx.navigate("/keys/" + encodeURIComponent(created.id)),
                }),
            },
            "Rotate",
          )
        : null,
      canRevoke(ctx, key) && isLive(key)
        ? h(
            "button",
            {
              class: "btn btn-danger",
              onClick: () =>
                confirmRevoke(
                  key,
                  () => ctx.navigate("/keys"),
                  inPeriod(since),
                ),
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
            [
              "User",
              person
                ? h("span", { class: "muted" }, person.email)
                : h("span", { class: "faint" }, NO_USER),
            ],
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
                : h("span", { class: "faint" }, `not ${inPeriod(since)}`),
            ],
            [
              "Guardrails",
              summarise(key.limits || {}).length
                ? h(
                    "span",
                    { class: "muted" },
                    summarise(key.limits).join(" · "),
                  )
                : h(
                    "span",
                    { class: "faint" },
                    `Inherited from ${project ? project.name : orgNameOf(ctx, key.org_id)}`,
                  ),
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
  const models = (await api.models(ctx.orgID)).data || [];
  const model = models.find((m) => m.alias === ctx.param);
  const canEdit = model ? isAdmin(ctx) : false;

  return screen(ctx, {
    back: { path: "/models", label: "Models" },
    title: ctx.param,
    mono: true,
    identity: model
      ? "serves " + servedName(model)
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
            ? pill(`Released ${releaseDay(model.release_date)}`)
            : null,
        ]
      : [pill("Deleted", "warn")],
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
              "aria-label": canEdit ? "Edit model" : null,
              onClick: () => openModel(ctx, model),
            },
            canEdit
              ? [icon(icons.pencil), "Edit"]
              : [icon(icons.models), "View model"],
          ),
          canEdit
            ? busyButton(
                { title: "Send a test request to this model's backend" },
                "Check",
                () => checkModel(model),
              )
            : null,
        ]
      : [],
    panels: (o, names) => [
      barPanel(
        "Projects by cost",
        o.top_projects,
        (r) => names.projects[r.group] || r.group || "No project",
        (r) => r.cost_micros,
        0,
        true,
      ),
      barPanel(
        "Keys by cost",
        o.top_keys,
        (r) => names.keys[r.group] || r.group || "No key",
        (r) => r.cost_micros,
        3,
        true,
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
                model.display_name
                  ? h(
                      "span",
                      {},
                      model.display_name,
                      h(
                        "span",
                        { class: "mono muted" },
                        ` - ${model.backend_model}`,
                      ),
                    )
                  : h("span", { class: "mono muted" }, model.backend_model),
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
                            class: "mono muted small",
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
                      isAdmin(ctx) ? "none" : "not shown to your role",
                    ),
              ],
              [
                "Price / Mtok (CHF)",
                model.input_micros_per_mtok || model.output_micros_per_mtok
                  ? h(
                      "span",
                      { class: "muted nowrap" },
                      `${listPrice(model.input_micros_per_mtok)} in · ` +
                        `${listPrice(model.output_micros_per_mtok)} out`,
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

/* ------------------------------------------------------------------ users */

export async function userDetailView(ctx) {
  const [usersRes, keysRes] = await Promise.all([
    api.users(ctx.orgID),
    api.keys(ctx.orgID).catch(() => ({ data: [] })),
  ]);
  const user = (usersRes.data || []).find((u) => u.id === ctx.param);

  if (!user) {
    return gone(
      ctx,
      "/users",
      "Users",
      "No such user",
      "No user has this id, or they belong to another organisation.",
    );
  }

  const keys = (keysRes.data || []).filter(
    (k) => k.user_id === user.id && stateOf(k) === "active",
  );
  // The same rules as the row on the Users screen: nobody edits themselves,
  // an operator's role comes from the configuration, and a role from the
  // directory is changed there.
  const editable = user.id !== ctx.state.me.user_id && user.role !== "operator";
  const canAssign = !ctx.state.me.roles_from_directory;

  return screen(ctx, {
    back: { path: "/users", label: "Users" },
    title: user.email,
    identity: signsInWith(user),
    scope: { user_id: user.id },
    // A person's traffic reads best as the tasks they ran.
    sessions: true,
    hide: { who: true },
    meta: [
      pill(roleLabel(user.role), TONE[user.role] || ""),
      user.disabled_at ? pill("Disabled", "bad") : null,
      pill(`${num(keys.length)} active ${plural(keys.length, "key")}`),
    ],
    actions: editable
      ? [
          canAssign && !user.disabled_at
            ? h(
                "button",
                {
                  class: "btn",
                  title: "Edit this user's role",
                  onClick: () => changeRole(ctx, user),
                },
                icon(icons.pencil),
                "Role",
              )
            : null,
          user.disabled_at
            ? h(
                "button",
                { class: "btn", onClick: () => enablePerson(ctx, user) },
                "Enable",
              )
            : h(
                "button",
                {
                  class: "btn btn-danger",
                  onClick: () => disablePerson(ctx, user),
                },
                "Disable",
              ),
        ]
      : [],
    panels: (o, names) => [
      barPanel(
        "Models by tokens",
        o.top_models,
        (r) => r.group || "unknown model",
        (r) => r.input_tokens + r.output_tokens,
        1,
      ),
      barPanel(
        "Keys by cost",
        o.top_keys,
        (r) => names.keys[r.group] || r.group || "No key",
        (r) => r.cost_micros,
        3,
        true,
      ),
    ],
    aside: h(
      "div",
      { class: "card" },
      h("div", { class: "card-head" }, h("h2", {}, "This user")),
      h(
        "div",
        { class: "card-body" },
        facts([
          [
            "Added",
            h(
              "span",
              { class: "muted", title: dateTime(user.created_at) },
              date(user.created_at),
            ),
          ],
          [
            "Last active",
            user.last_active_at
              ? h(
                  "span",
                  {
                    class: "muted",
                    title:
                      dateTime(user.last_active_at) +
                      " - last sign-in or request through one of their keys",
                  },
                  ago(user.last_active_at),
                )
              : h("span", { class: "faint" }, "never"),
          ],
          user.disabled_at
            ? [
                "Disabled",
                h(
                  "span",
                  { class: "muted", title: dateTime(user.disabled_at) },
                  date(user.disabled_at),
                ),
              ]
            : null,
          [
            "Active keys",
            keys.length
              ? h(
                  "div",
                  { class: "stack" },
                  keys.map((k) =>
                    h(
                      "a",
                      {
                        href: "/keys/" + encodeURIComponent(k.id),
                        onClick: go(ctx, "/keys/" + encodeURIComponent(k.id)),
                      },
                      k.name,
                    ),
                  ),
                )
              : h("span", { class: "faint" }, "none"),
          ],
        ]),
      ),
    ),
    note: "Counts the requests made with this user's keys.",
  });
}

/* ----------------------------------------------------------- the scaffold */

// screen draws all four. Everything above the chart is what makes this entity
// that entity; everything from the chart down is the same report every time.
// spec.description is what the thing says about itself, under the header;
// spec.note is how to read the numbers, at the bottom.
async function screen(ctx, spec) {
  const since = currentRange();
  const res = await api.overview(ctx.orgID, since, spec.scope);
  const o = res.overview;
  const names = namesFrom(res);

  ctx.setTitle(spec.title, spec.identity || "");

  const head = detailHead(ctx, [spec.back.path, spec.back.label], spec.meta, [
    rangePicker(ctx, since),
    spec.actions,
  ]);

  const wrap = h("div", {}, head);
  if (spec.description) {
    wrap.append(
      h(
        "p",
        { class: "muted", style: { margin: "0 0 16px" } },
        spec.description,
      ),
    );
  }
  if (spec.banner) {
    wrap.append(
      banner("warn", { style: { marginBottom: "16px" } }, spec.banner),
    );
  }

  wrap.append(
    h(
      "div",
      { class: "grid grid-4" },
      stat(
        "Requests",
        compact(o.requests),
        o.refused ? `${num(o.refused)} refused` : "none refused",
      ),
      stat(
        "Tokens",
        compact(o.input_tokens + o.output_tokens),
        `${compact(o.input_tokens)} in · ${compact(o.output_tokens)} out`,
      ),
      moneyStat("Cost", o.cost_micros),
      stat("First token", ms(o.ttft_median_ms), `p95 ${ms(o.ttft_p95_ms)}`),
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
                `${num(o.failed)} failed (${pct(o.failed, o.requests)})`,
                spec.sessions,
              )
            : h(
                "span",
                { class: "pill pill-good" },
                h("span", { class: "dot" }),
                "No failures",
              ),
        ),
        h(
          "div",
          { class: "card-body" },
          o.requests === 0
            ? empty(
                "Nothing recorded in this period",
                spec.sessions
                  ? "Pick a longer period."
                  : "Pick a longer period, or see the refused requests below.",
              )
            : areaChart(o.series, { bucket: o.bucket }),
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
// shows the sessions holding a failed request.
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
        ? "Show only the sessions with a failed request below"
        : "Show only the failed requests below",
      onClick: () => {
        setOutcome("failed");
        ctx.reload();
      },
    },
    h("span", { class: "dot" }),
    text,
  );
}

function barPanel(title, rows, label, value, colorIndex, cost) {
  return h(
    "div",
    { class: "card" },
    h("div", { class: "card-head" }, h("h2", {}, title)),
    h(
      "div",
      { class: "card-body" },
      barList(rows || [], { label, value, colorIndex, cost }),
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

function budgetPill(ctx, project) {
  if (!project.budget_micros) {
    return pill(`${money(project.spend_micros)} this month, no budget`);
  }
  const frac = project.spend_micros / project.budget_micros;
  const tone = frac >= 1 ? "bad" : frac >= 0.8 ? "warn" : "";
  return h(
    "span",
    { class: "pill" },
    meter(frac, tone),
    `${money(project.spend_micros)} of ${money(project.budget_micros)} per ${project.period}`,
  );
}

// gone is the screen for an id that names nothing. It is an answer rather than
// an error: the usual way to reach one is a bookmark from before the thing was
// deleted, and "it is not there any more" is what the reader needs to be told.
