// Sandboxes: the machines this gateway lends out.
//
// Every other screen in this panel is about a request. This one is about a
// machine, and it belongs here because the machine is where the quota, the
// bill and the guardrail already are: a sandbox is asked for by a person in an
// organisation, paid for out of that organisation's capacity, and the agent
// inside it makes requests on a key this gateway minted.
//
// Two things had to be visible rather than derivable, and the layout is built
// around them.
//
// The first is when a sandbox goes away - the property that distinguishes this
// from a virtual machine. It is a column rather than a detail, written as a
// time remaining rather than a timestamp, because "in 40m" is what somebody
// acts on.
//
// The second is what a sandbox actually is, which is not what it was asked
// for: a class can be changed under a running sandbox, so every row shows the
// machine it was given out of its own row rather than out of the catalogue.
//
// What is deliberately not here is a way in. An administrator can see every
// sandbox in the organisation and terminate one, because the quota and the bill
// are theirs, and cannot open a shell in one. Attaching is
// `keera sandbox ssh`, for the owner alone.

import { api } from "../api.js";
import {
  h,
  table,
  modal,
  confirm,
  toast,
  pill,
  stat,
  empty,
  field,
  showError,
  duration,
  dateTime,
  ago,
  compact,
  num,
  icon,
  icons,
} from "../ui.js";
import { chooseOrg } from "./orgs.js";

export async function sandboxesView(ctx) {
  // A sandbox belongs to an organisation, because its quota and its bill do.
  if (!ctx.orgID) return chooseOrg(ctx, "Sandboxes");

  const me = ctx.state.me;
  const canAdmin = me.can_admin_org || me.unrestricted;
  const [list, cat, teams] = await Promise.all([
    api.sandboxes(ctx.orgID, { all: showAll() }),
    api.sandboxClasses(ctx.orgID),
    // Only an administrator may put a sandbox on a team, so only an
    // administrator's dialog needs the list. A member's sandbox is created
    // against the organisation's own guardrails, and asking them to choose
    // from a list they cannot use would be a field that only ever refuses.
    canAdmin ? api.teams(ctx.orgID).catch(() => ({ data: [] })) : { data: [] },
  ]);
  ctx.state.teams = teams.data || [];
  const sandboxes = list.data || [];
  const classes = cat.data || [];
  const driver = cat.driver || null;
  const limits = cat.limits || null;

  ctx.setSubtitle(
    `${sandboxes.length} sandbox${sandboxes.length === 1 ? "" : "es"}`,
  );

  // A deployment with no driver gets the one sentence somebody can act on
  // rather than an empty table, which would read as "nobody is using this".
  //
  // The two cases are worded apart on purpose. A deployment that has declared
  // nothing has not started; a deployment whose catalogue is full and whose
  // driver is unset has done half the configuration, and the half it did is the
  // visible one - so telling that reader "this deployment lends out no
  // sandboxes" underneath a table of sandbox classes reads as a panel that
  // cannot see its own state.
  if (!driver) {
    return h(
      "div",
      { class: "grid" },
      h(
        "div",
        { class: "card" },
        classes.length
          ? empty(
              "Classes are set up, but no driver can start them",
              h(
                "div",
                {},
                "No sandbox driver is running, so every start is refused. " +
                  "Set KEERA_SANDBOX_DRIVER to 'podman' on a single host, or " +
                  "to 'kubernetes' with the agent-sandbox controller on the " +
                  "cluster. See docs/sandboxes.md.",
              ),
            )
          : empty(
              "Sandboxes are not turned on",
              h(
                "div",
                {},
                "A sandbox is a machine with the toolchain installed, its own " +
                  "API key that never leaves it, and network access only to " +
                  "places the deployment allows. To turn them on, set " +
                  "KEERA_SANDBOX_DRIVER and a catalogue in " +
                  "KEERA_SANDBOXES_FILE. See docs/sandboxes.md.",
              ),
            ),
      ),
      classes.length ? classCard(ctx, classes, null, me) : null,
    );
  }

  const live = sandboxes.filter((s) => isLive(s));
  const running = live.filter((s) => s.state === "ready").length;
  const coreSeconds = sandboxes.reduce(
    (sum, s) => sum + Math.round((s.running_seconds * s.cpu_millis) / 1000),
    0,
  );

  return h(
    "div",
    { class: "grid" },
    head(ctx, classes, driver, limits, canAdmin, teams.data || []),
    h(
      "div",
      { class: "grid grid-4" },
      stat("Live", num(live.length), null, limitNote(limits)),
      stat("Running now", num(running), null, "the rest are suspended"),
      // Core-seconds rather than wall time, because two minutes of thirty-two
      // cores is not two minutes of two - and this is the number a chargeback
      // uses. The wall time is on each row, where it can be read against the
      // machine it belongs to.
      stat(
        "Compute used",
        compact(Math.round(coreSeconds / 60)),
        "core-min",
        "over the sandboxes listed",
      ),
      stat(
        "Isolation",
        driver.isolation,
        null,
        `the strongest the ${driver.name} driver offers`,
      ),
    ),
    listCard(ctx, sandboxes, canAdmin, me),
    classCard(ctx, classes, driver, me),
  );
}

/** The strip above the table: what a sandbox is, and the button that makes one. */
function head(ctx, classes, driver, limits, canAdmin, teams) {
  const usable = classes.filter((c) => allowed(c, limits, driver));
  return h(
    "div",
    { class: "detail-head" },
    h(
      "div",
      { class: "muted" },
      "A sandbox is a machine for one task or one working day. It has the " +
        "toolchain installed, a home directory that survives suspension, its " +
        "own API key that never leaves it, and network access only to places " +
        "this deployment allows. It expires on its own. Only its owner can " +
        "open a shell in it: 'keera sandbox ssh <name>', over this same port.",
    ),
    h(
      "div",
      { class: "row", style: { flexWrap: "wrap" } },
      h("div", { style: { flex: 1 } }),
      usable.length
        ? h(
            "button",
            {
              class: "btn btn-primary",
              onClick: () => newSandbox(ctx, usable, limits, teams),
            },
            icon(icons.plus),
            "New sandbox",
          )
        : null,
    ),
  );
}

function limitNote(limits) {
  if (!limits || !limits.max_sandboxes) return "no limit set";
  return `of ${limits.max_sandboxes} allowed here`;
}

/** The sandboxes themselves. */
function listCard(ctx, sandboxes, canAdmin, me) {
  return table(
    [
      {
        label: "Name",
        cell: (s) =>
          h(
            "div",
            {},
            h("strong", { class: "mono" }, s.name),
            s.repo
              ? h(
                  "div",
                  { class: "faint", style: { fontSize: "11.5px" } },
                  s.repo,
                )
              : null,
          ),
        sortKey: (s) => s.name,
      },
      {
        label: "State",
        shrink: true,
        cell: (s) =>
          h(
            "div",
            { class: "row-tight" },
            pill(s.state, stateTone(s.state)),
            s.purpose === "agent" ? pill("agent", "accent") : null,
          ),
        sortKey: (s) => s.state,
      },
      {
        label: "Class",
        shrink: true,
        cell: (s) =>
          h(
            "div",
            {},
            h("span", { class: "mono muted" }, s.class),
            // The machine it was actually given, not the machine the class
            // currently describes. A class can be changed under a running
            // sandbox, and a row that joined would describe one that never ran.
            h(
              "div",
              { class: "faint", style: { fontSize: "11.5px" } },
              `${size(s)} · ${s.isolation || "standard"}`,
            ),
          ),
        sortKey: (s) => s.class,
      },
      {
        label: "Owner",
        cell: (s) =>
          h(
            "div",
            {},
            h("span", { class: "muted" }, s.owner || "—"),
            // The team, where there is one. It belongs beside the owner rather
            // than in a column of its own: what it answers is "whose is this",
            // and a team is the other half of that - it is the budget the
            // sandbox is charged to and the guardrail its agent works under.
            s.team_id
              ? h(
                  "div",
                  { class: "faint", style: { fontSize: "11.5px" } },
                  teamName(ctx, s.team_id),
                )
              : null,
          ),
        sortKey: (s) => s.owner || "",
      },
      {
        label: "Ran for",
        shrink: true,
        num: true,
        cell: (s) => h("span", {}, duration(s.running_seconds * 1000)),
        sortKey: (s) => s.running_seconds,
        sortDir: "desc",
      },
      {
        // The column this screen exists for. A time remaining rather than a
        // timestamp, because "in 40m" is the form somebody acts on and
        // "2026-09-14T16:20:00Z" is one they have to do arithmetic on.
        label: "Expires",
        shrink: true,
        num: true,
        cell: (s) => expiryCell(s),
        sortKey: (s) => s.expires_at || "",
      },
      {
        label: "",
        shrink: true,
        cell: (s) => actions(ctx, s, canAdmin, me),
      },
    ],
    sandboxes,
    {
      search: (s) => `${s.name} ${s.class} ${s.owner} ${s.repo}`,
      searchLabel: "sandboxes",
      // Not a filter over the rows here: a terminated sandbox is not in this
      // list until the control plane is asked for it, because the list is
      // capped and the live ones are what the cap is for.
      toggles: [
        {
          label: "Show terminated",
          on: showAll(),
          onChange: (v) => setShowAll(v, ctx),
        },
      ],
      sortBy: "Expires",
      rowClass: (s) => (isLive(s) ? "" : "row-faint"),
      emptyTitle: "No sandboxes",
      emptyBody:
        "Start one with the button above, or with " +
        "'keera sandbox up <name> --class <class>'.",
    },
  );
}

/** The catalogue, which is the deployment's rather than the organisation's. */
function classCard(ctx, classes, driver, me) {
  const canEdit = me.can_edit_catalogue;
  return h(
    "div",
    { class: "stack", style: { gap: "10px" } },
    h(
      "h2",
      { style: { marginBottom: "0" } },
      "Classes",
      h("span", { class: "faint" }, " — the machines this deployment offers"),
    ),
    table(
      [
        {
          label: "Name",
          cell: (c) =>
            h(
              "div",
              {},
              h("strong", { class: "mono" }, c.name),
              c.description
                ? h(
                    "div",
                    { class: "faint", style: { fontSize: "11.5px" } },
                    c.description,
                  )
                : null,
            ),
        },
        {
          label: "Isolation",
          shrink: true,
          cell: (c) => isolationCell(c, driver),
        },
        {
          label: "Size",
          shrink: true,
          num: true,
          cell: (c) =>
            h("span", { class: "mono" }, sizeOf(c.cpu_millis, c.memory_mib)),
        },
        {
          label: "Volume",
          shrink: true,
          num: true,
          cell: (c) =>
            c.disk_mib
              ? h("span", { class: "mono" }, gib(c.disk_mib))
              : h("span", { class: "faint" }, "none"),
        },
        {
          label: "Lifetime",
          shrink: true,
          num: true,
          cell: (c) =>
            h(
              "span",
              { class: "mono" },
              `${duration(c.default_ttl / 1e6)} / ${duration(c.max_ttl / 1e6)}`,
            ),
        },
        {
          label: "Warm",
          shrink: true,
          num: true,
          cell: (c) =>
            c.warm
              ? h("span", { class: "mono" }, num(c.warm))
              : h("span", { class: "faint" }, "—"),
        },
        {
          label: "Source",
          shrink: true,
          cell: (c) =>
            c.managed
              ? pill("catalogue file", "")
              : h("span", { class: "faint" }, "control plane"),
        },
        {
          label: "",
          shrink: true,
          cell: (c) =>
            canEdit && !c.managed
              ? h(
                  "button",
                  {
                    class: "btn btn-ghost btn-sm",
                    title: "Delete this class",
                    onClick: () => removeClass(ctx, c),
                  },
                  icon(icons.trash),
                )
              : null,
        },
      ],
      classes,
      {
        emptyTitle: "No sandbox classes",
        emptyBody:
          "Classes come from KEERA_SANDBOXES_FILE, which is applied on " +
          "every start.",
      },
    ),
  );
}

/** isolationCell says both what a class asks for and whether it can be given. */
function isolationCell(c, driver) {
  const order = ["standard", "isolated", "vm"];
  const deliverable =
    !driver || order.indexOf(driver.isolation) >= order.indexOf(c.isolation);
  if (deliverable) return pill(c.isolation, c.isolation === "vm" ? "good" : "");
  // A class this deployment cannot deliver is refused at creation rather than
  // quietly run at a weaker tier, so saying so here is the difference between
  // a confusing refusal later and a configuration change now.
  return h(
    "div",
    { class: "row-tight" },
    pill(c.isolation, "warn"),
    h(
      "span",
      {
        class: "faint",
        title:
          "No RuntimeClass is mapped to this tier, so this class cannot start",
      },
      "not available",
    ),
  );
}

/* ------------------------------------------------------------------ actions */

function actions(ctx, s, canAdmin, me) {
  const mine = s.user_id && s.user_id === me.user_id;
  if (!isLive(s) || (!canAdmin && !mine)) return null;
  const btns = [];
  if (s.state === "suspended" || s.state === "expired") {
    btns.push(
      h(
        "button",
        {
          class: "btn btn-ghost btn-sm",
          title: "Resume it with its volume intact",
          onClick: () =>
            act(ctx, () => api.resumeSandbox(s.id), `${s.name} is resuming`),
        },
        "Resume",
      ),
    );
  } else if (s.state === "ready") {
    btns.push(
      h(
        "button",
        {
          class: "btn btn-ghost btn-sm",
          title: "Release its compute and keep its volume",
          onClick: () =>
            act(ctx, () => api.suspendSandbox(s.id), `${s.name} is suspending`),
        },
        "Suspend",
      ),
    );
  }
  btns.push(
    h(
      "button",
      {
        class: "btn btn-ghost btn-sm",
        title: "Extend its expiry",
        onClick: () => extend(ctx, s),
      },
      "Extend",
    ),
    h(
      "button",
      {
        class: "btn btn-sm btn-danger",
        title: "Terminate it and its volume",
        onClick: () => terminateSandbox(ctx, s),
      },
      icon(icons.trash),
    ),
  );
  return h("div", { class: "row-tight" }, ...btns);
}

async function act(ctx, fn, message) {
  try {
    await fn();
    toast(message);
    ctx.reload();
  } catch (e) {
    toast(e.message, "bad");
  }
}

function extend(ctx, s) {
  const ttl = h("input", { class: "input", value: "4h", placeholder: "4h" });
  const err = h("div");
  modal({
    title: `Extend ${s.name}`,
    subtitle:
      "Counted from now, not added to the time left. The same limits as at " +
      "creation apply.",
    body: h("div", {}, err, field("Lifetime from now", ttl)),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Cancel"),
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: async (e) => {
            const button = e.currentTarget;
            button.disabled = true;
            try {
              const res = await api.extendSandbox(s.id, ttl.value.trim());
              close();
              toast(
                `${s.name} now expires ${dateTime(res.expires_at)}`,
                "good",
              );
              ctx.reload();
            } catch (ex) {
              showError(err, ex.message);
              button.disabled = false;
            }
          },
        },
        "Extend",
      ),
    ],
  });
}

function terminateSandbox(ctx, s) {
  confirm({
    title: `Terminate ${s.name}?`,
    body: h(
      "div",
      { class: "stack" },
      h(
        "div",
        {},
        s.disk_mib
          ? "Its home directory goes with it. Anything not pushed is lost."
          : "It has no volume, so only what is in memory is lost.",
      ),
      h(
        "div",
        { class: "faint" },
        "Its API key is revoked. Its cost stays in the usage report.",
      ),
    ),
    confirmLabel: "Terminate",
    danger: true,
    onConfirm: async () => {
      await api.terminateSandbox(s.id);
      toast(`${s.name} terminated`);
      ctx.reload();
    },
  });
}

function removeClass(ctx, c) {
  confirm({
    title: `Delete the class ${c.name}?`,
    body: h(
      "div",
      {},
      "Running sandboxes keep their image and resources, so nothing " +
        "breaks. New sandboxes can no longer use this class.",
    ),
    confirmLabel: "Delete",
    danger: true,
    onConfirm: async () => {
      await api.deleteSandboxClass(c.name);
      toast(`${c.name} deleted`);
      ctx.reload();
    },
  });
}

/** newSandbox is the create dialog.
 *
 *  It asks for four things and no more. The ssh key is the one that would
 *  otherwise be a surprise: a sandbox with nothing in authorized_keys is one
 *  nobody can open a shell in, so the field is required for an engineer's
 *  sandbox and the dialog says why rather than letting the control plane
 *  refuse after everything else has been filled in. */
function newSandbox(ctx, classes, limits, teams) {
  const name = h("input", { class: "input", placeholder: "fix-login" });
  const cls = h(
    "select",
    { class: "input" },
    ...classes.map((c) =>
      h(
        "option",
        { value: c.name },
        `${c.name} — ${sizeOf(c.cpu_millis, c.memory_mib)}, ${c.isolation}`,
      ),
    ),
  );
  // Which team the sandbox - and the key minted for it - belongs to.
  //
  // It is the field with the most behind it: the team decides the sandbox's
  // quota, its budget, its rate limit, the system prompt its agent carries and
  // which models that agent may reach. Empty is the organisation's own
  // guardrails, which is what a sandbox got before this existed.
  const team = h(
    "select",
    { class: "input" },
    h("option", { value: "" }, "No team — the organisation's own guardrails"),
    ...teams.map((t) => h("option", { value: t.id }, t.name)),
  );
  const repo = h("input", {
    class: "input",
    placeholder: "https://git.example.internal/team/service.git",
  });
  const branch = h("input", { class: "input", placeholder: "main" });
  const ttl = h("input", {
    class: "input",
    placeholder: "the class's default",
  });
  const key = h("textarea", {
    class: "input",
    rows: 3,
    placeholder: "ssh-ed25519 AAAA… you@laptop",
  });
  const err = h("div");

  modal({
    title: "New sandbox",
    subtitle:
      "It gets its own API key, scoped to you and revoked with the sandbox.",
    wide: true,
    body: h(
      "div",
      {},
      err,
      field(
        "Name",
        name,
        "Lowercase letters, digits and hyphens. It becomes the hostname: " +
          "keera sandbox ssh <name>.",
      ),
      field("Class", cls),
      teams.length
        ? field(
            "Team",
            team,
            "The sandbox's key is scoped to this team, and the sandbox " +
              "counts toward its quota.",
          )
        : null,
      field(
        "Your ssh public key (required)",
        key,
        "The key that can open a shell in it. 'keera sandbox up' sends " +
          "yours from ~/.ssh automatically.",
      ),
      field(
        "Repository",
        repo,
        "Optional. Cloned with a credential made for this sandbox only.",
      ),
      field("Branch", branch),
      field(
        "Lifetime",
        ttl,
        limits && limits.max_sandbox_ttl_seconds
          ? `Capped at ${duration(limits.max_sandbox_ttl_seconds * 1000)} here.`
          : "Leave empty for the class default.",
      ),
    ),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Cancel"),
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: async (e) => {
            const button = e.currentTarget;
            // What the control plane would refuse anyway, refused here - so
            // that the answer arrives beside the field that is wrong instead of
            // after a round trip, and so that the field gets focus rather than
            // the reader having to find it.
            const keys = key.value
              .split("\n")
              .map((l) => l.trim())
              .filter((l) => l && !l.startsWith("#"));
            for (const check of [
              [!name.value.trim(), name, "Give the sandbox a name."],
              [
                !keys.length,
                key,
                "Paste an ssh public key. Without one, nobody can open a " +
                  "shell in this sandbox. 'cat ~/.ssh/id_ed25519.pub' prints " +
                  "yours.",
              ],
            ]) {
              if (check[0]) {
                showError(err, check[2]);
                check[1].focus();
                return;
              }
            }
            button.disabled = true;
            try {
              const sb = await api.createSandbox({
                org_id: ctx.orgID,
                name: name.value.trim(),
                class: cls.value,
                team_id: team.value,
                repo: repo.value.trim(),
                branch: branch.value.trim(),
                ttl: ttl.value.trim(),
                authorized_keys: keys,
              });
              close();
              toast(`${sb.name} is starting`, "good");
              ctx.reload();
            } catch (ex) {
              showError(err, ex.message);
              button.disabled = false;
            }
          },
        },
        "Create",
      ),
    ],
  });
}

/* ------------------------------------------------------------------ helpers */

/** teamName is a team's name where the screen knows it, and its id otherwise.
 *
 *  A member's list is narrowed to their own sandboxes and they cannot read the
 *  team list, so the id is what they get - which is still better than nothing,
 *  because it is the string their administrator will ask them for. */
function teamName(ctx, id) {
  const known = (ctx.state.teams || []).find((t) => t.id === id);
  return known ? known.name : id;
}

function isLive(s) {
  return (
    s.state === "pending" || s.state === "ready" || s.state === "suspended"
  );
}

function stateTone(state) {
  switch (state) {
    case "ready":
      return "good";
    case "pending":
      return "accent";
    case "suspended":
      return "warn";
    case "failed":
      return "bad";
    default:
      return "";
  }
}

function expiryCell(s) {
  if (!s.expires_at) return h("span", { class: "faint" }, "never");
  const left = new Date(s.expires_at) - Date.now();
  if (left <= 0) {
    return h(
      "span",
      { class: "faint", title: dateTime(s.expires_at) },
      ago(s.expires_at),
    );
  }
  // The last half hour is the one worth colouring: it is the window in which
  // somebody can still do something about it.
  const tone = left < 30 * 60 * 1000 ? "warn" : "";
  return h(
    "span",
    { class: tone ? "pill pill-warn" : "mono", title: dateTime(s.expires_at) },
    "in " + duration(left),
  );
}

function size(s) {
  return sizeOf(s.cpu_millis, s.memory_mib);
}

function sizeOf(cpuMillis, memoryMiB) {
  const cores =
    cpuMillis % 1000 === 0 ? cpuMillis / 1000 : (cpuMillis / 1000).toFixed(1);
  return `${cores} core${cores === 1 ? "" : "s"}, ${gib(memoryMiB)}`;
}

function gib(mib) {
  if (!mib) return "0";
  return mib % 1024 === 0 ? `${mib / 1024}Gi` : `${mib}Mi`;
}

/** allowed reports whether this reader could actually be given this class, so
 *  the create dialog offers only the ones that would not be refused. */
function allowed(c, limits, driver) {
  const order = ["standard", "isolated", "vm"];
  if (driver && order.indexOf(driver.isolation) < order.indexOf(c.isolation)) {
    return false;
  }
  if (c.purposes && c.purposes.length && !c.purposes.includes("engineer")) {
    return false;
  }
  if (!limits) return true;
  if (limits.sandbox_classes && !limits.sandbox_classes.includes(c.name)) {
    return false;
  }
  if (
    limits.max_sandbox_cpu_millis &&
    c.cpu_millis > limits.max_sandbox_cpu_millis
  ) {
    return false;
  }
  if (
    limits.max_sandbox_memory_mib &&
    c.memory_mib > limits.max_sandbox_memory_mib
  ) {
    return false;
  }
  return true;
}

/** setShowAll remembers the choice and asks for the list again.
 *
 *  Per-viewer and per-browser, which is right for a view preference: whether
 *  somebody wants to see terminated sandboxes is a fact about how they are
 *  reading the screen, not about the deployment. */
function setShowAll(on, ctx) {
  try {
    localStorage.setItem("keera.sandboxes.all", on ? "1" : "0");
  } catch {
    /* a browser with site data blocked still gets the default */
  }
  ctx.reload();
}

function showAll() {
  try {
    return localStorage.getItem("keera.sandboxes.all") === "1";
  } catch {
    return false;
  }
}
