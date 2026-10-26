// One project, key, model or user: the dashboard narrowed to it, and the
// requests or sessions behind the numbers. All four are one screen with a
// different scope, so their totals cannot drift apart.

import { api } from "../api.js";
import {
  h,
  stat,
  pill,
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
  personName,
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
import { openModel, checkModel, deleteModel, servedName } from "./models.js";
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
import { budgetUse } from "./limits.js";
import {
  TONE,
  roleLabel,
  signsInWith,
  changeRole,
  disablePerson,
  enablePerson,
} from "./people.js";

export async function projectDetailView(ctx) {
  const projectsRes = await api.projects(ctx.orgID);
  const project = (projectsRes.data || []).find((t) => t.id === ctx.param);
  // The project's own organisation; the switcher may show "All
  // organisations".
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
        `${num(project.active_keys)} active ${plural(project.active_keys, "key")}`,
      ),
    ],
    actions: [
      busyButton(
        { title: "Guardrails for this project's keys" },
        [icon(icons.sliders), canEdit ? "Guardrails" : "View guardrails"],
        () => openProject(ctx, project, models, orgID, orgName, canEdit),
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
    panels: (o, names) => [modelsPanel(o), keysPanel(o, names)],
    aside: h(
      "div",
      { class: "card" },
      h("div", { class: "card-head" }, h("h2", {}, "What this project allows")),
      h(
        "div",
        { class: "card-body" },
        facts([
          ["Models", modelsCell(project.limits)],
          ["Budget", projectBudget(project)],
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
      "narrow what the organisation allows.",
  });
}

export async function keyDetailView(ctx) {
  // So Last used covers the same span as the numbers below it.
  const since = currentRange();
  const [keysRes, projectsRes, usersRes] = await Promise.all([
    api.keys(ctx.orgID, { since }),
    api.projects(ctx.orgID).catch(() => ({ data: [] })),
    api.users(ctx.orgID).catch(() => ({ data: [] })),
  ]);
  const key = (keysRes.data || []).find((k) => k.id === ctx.param);
  // The key's own organisation, not the switcher's.
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
    // A key is one person or pipeline, so its traffic reads best as tasks.
    sessions: true,
    // Both columns would repeat the key.
    hide: { key: true, project: true },
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
      // Only an admin has the Users screen.
      person && isAdmin(ctx)
        ? h(
            "a",
            {
              class: "pill pill-button",
              href: "/users/" + encodeURIComponent(person.id),
              title: "Open this key's user",
              onClick: go(ctx, "/users/" + encodeURIComponent(person.id)),
            },
            personName(person),
          )
        : person
          ? pill(personName(person))
          : null,
    ],
    actions: [
      busyButton(
        { title: "Guardrails for this key" },
        [icon(icons.sliders), canEdit ? "Guardrails" : "View guardrails"],
        () => openKey(ctx, key, models, project ? project.name : "", canEdit),
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
      // Rotation revokes the old key, so the screen moves to the new one.
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
      modelsPanel(o),
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
                ? h("span", { class: "muted" }, personName(person))
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
          model.max_context_tokens
            ? pill(`${compact(model.max_context_tokens)} context`)
            : null,
          model.location ? pill(locationName(model.location)) : null,
          model.release_date
            ? pill(`Released ${releaseDay(model.release_date)}`)
            : null,
        ]
      : [pill("Deleted", "warn")],
    // A removed model still has traffic, which is usually why someone is
    // here, so the screen still opens.
    banner: model
      ? null
      : `This alias is no longer one of ${orgNameOf(ctx)}'s models. Requests for it are ` +
        "refused with a 404. Its history is below.",
    actions: model
      ? [
          canEdit
            ? busyButton(
                { title: "Send a test request to this model's backend" },
                "Check",
                () => checkModel(model),
              )
            : null,
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
            ? h(
                "button",
                {
                  class: "btn btn-danger",
                  title: "Delete this model",
                  onClick: () =>
                    deleteModel(ctx, model, () => ctx.navigate("/models")),
                },
                icon(icons.trash),
                "Delete",
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
      keysPanel(o, names),
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
                      // Backends are for admins, so an empty list does not
                      // mean there are none.
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
  // Same rules as the Users screen: nobody edits themselves, an operator's
  // role comes from the configuration, and a directory role is changed there.
  const editable = user.id !== ctx.state.me.user_id && user.role !== "operator";
  const canAssign = !ctx.state.me.roles_from_directory;

  return screen(ctx, {
    back: { path: "/users", label: "Users" },
    title: personName(user),
    identity: user.name
      ? `${user.email}, ${signsInWith(user)}`
      : signsInWith(user),
    scope: { user_id: user.id },
    // A person's traffic reads best as the tasks they ran.
    sessions: true,
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
    panels: (o, names) => [modelsPanel(o), keysPanel(o, names)],
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

// Above the chart differs per entity; from the chart down is the same report.
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

  // The chart's failure count narrows the log below in place.
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
            ? empty("Nothing recorded in this period", "Pick a longer period.")
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

  // Only an admin gets the log here. A member's log holds only their own
  // keys' requests, which would not match the charts.
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

// Under a session log it shows the sessions holding a failed request.
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

const modelsPanel = (o) =>
  barPanel(
    "Models by tokens",
    o.top_models,
    (r) => r.group || "unknown model",
    (r) => r.input_tokens + r.output_tokens,
    1,
  );

const keysPanel = (o, names) =>
  barPanel(
    "Keys by cost",
    o.top_keys,
    (r) => names.keys[r.group] || r.group || "No key",
    (r) => r.cost_micros,
    3,
    true,
  );

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

export function routerLinks(ctx, routers) {
  if (!routers.length) return h("span", { class: "muted" }, "none");
  return h(
    "div",
    { class: "wrap-chips" },
    routers.map((p) => {
      const path = "/routers/" + encodeURIComponent(p.alias);
      return h(
        "a",
        { class: "pill mono", href: path, onClick: go(ctx, path) },
        p.alias,
      );
    }),
  );
}

// Without a budget the month's spend is still worth seeing.
function projectBudget(project) {
  if (!project.budget_micros) {
    return h(
      "span",
      { class: "faint" },
      `None · ${money(project.spend_micros)} this month`,
    );
  }
  return budgetUse(project);
}
