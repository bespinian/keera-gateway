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
  field,
  showError,
  duration,
  dateTime,
  ago,
  compact,
  num,
  icon,
  icons,
  plural,
  isAdmin,
  sandboxNameProblem,
  listHead,
} from "../ui.js";
import { chooseOrg, orgNameOf } from "./orgs.js";
import { oldestProject } from "./projects.js";

export async function sandboxesView(ctx) {
  // A sandbox belongs to an organisation, because its quota and its bill do.
  if (!ctx.orgID) return chooseOrg(ctx, "Sandboxes");

  const me = ctx.state.me;
  const canAdmin = isAdmin(ctx);
  const [list, cat, projects] = await Promise.all([
    api.sandboxes(ctx.orgID, { all: showAll() }),
    api.sandboxClasses(ctx.orgID),
    // Only an administrator may put a sandbox on a project, so only an
    // administrator's dialog needs the list. A member's sandbox goes in the
    // organisation's oldest project, and asking them to choose
    // from a list they cannot use would be a field that only ever refuses.
    canAdmin
      ? api.projects(ctx.orgID).catch(() => ({ data: [] }))
      : { data: [] },
  ]);
  ctx.state.projects = projects.data || [];
  const sandboxes = list.data || [];
  const classes = cat.data || [];
  const driver = cat.driver;
  const limits = cat.limits || null;

  ctx.setSubtitle(
    `${sandboxes.length} ${plural(sandboxes.length, "sandbox", "sandboxes")}`,
  );

  const live = sandboxes.filter((s) => isLive(s));
  const running = live.filter((s) => s.state === "ready").length;
  const coreSeconds = sandboxes.reduce(
    (sum, s) => sum + Math.round((s.running_seconds * s.cpu_millis) / 1000),
    0,
  );

  return h(
    "div",
    { class: "grid" },
    head(ctx, classes, driver, limits, canAdmin, projects.data || []),
    h(
      "div",
      { class: "grid grid-4" },
      stat("Live", num(live.length), null, limitNote(limits, canAdmin)),
      stat(
        "Running now",
        num(running),
        null,
        "the rest are suspended or expired",
      ),
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
    listCard(ctx, sandboxes, canAdmin, me, driver),
    classCard(ctx, classes, driver),
  );
}

/** The strip above the table: what a sandbox is, and the button that makes one. */
function head(ctx, classes, driver, limits, canAdmin, projects) {
  const usable = classes.filter((c) => allowed(c, limits, driver));
  return listHead(
    "A sandbox is a machine for one task or one working day, with the " +
      "toolchain installed and its own API key. It expires on its own. Only " +
      "its owner can open a shell in it, with 'keera sandbox ssh <name>'.",
    [],
    usable.length
      ? {
          label: "Create sandbox",
          onClick: () => newSandbox(ctx, usable, limits, projects),
        }
      : null,
  );
}

// limitNote compares the count with the quota. The quota is the whole
// organisation's, but a member's list holds only their own sandboxes.
function limitNote(limits, canAdmin) {
  if (!limits || !limits.max_sandboxes) return "no limit set";
  if (!canAdmin)
    return `yours; the organisation allows ${limits.max_sandboxes} in all`;
  return `of ${limits.max_sandboxes} allowed here`;
}

/** The sandboxes themselves. */
function listCard(ctx, sandboxes, canAdmin, me, driver) {
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
            {},
            h(
              "div",
              { class: "row-tight" },
              pill(stateLabel(s.state), stateTone(s.state)),
              s.purpose === "agent" ? pill("Agent", "accent") : null,
            ),
            // Why it is stuck or failed. The other states explain themselves.
            (s.state === "pending" || s.state === "failed") && s.detail
              ? h(
                  "div",
                  { class: "faint", style: { fontSize: "11.5px" } },
                  s.detail,
                )
              : null,
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
            // The project, where there is one. It belongs beside the owner rather
            // than in a column of its own: what it answers is "whose is this",
            // and a project is the other half of that - it is the budget the
            // sandbox is charged to and the guardrail its agent works under.
            s.project_id
              ? h(
                  "div",
                  { class: "faint", style: { fontSize: "11.5px" } },
                  projectName(ctx, s.project_id),
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
        cell: (s) => actions(ctx, s, canAdmin, me, driver),
      },
    ],
    sandboxes,
    {
      search: (s) => `${s.name} ${s.class} ${s.owner} ${s.repo}`,
      searchLabel: "sandboxes",
      // Not a filter over the rows here: a finished sandbox is not in this
      // list until the control plane is asked for it, because the list is
      // capped and the live ones are what the cap is for.
      toggles: [
        {
          label: "Show finished",
          on: showAll(),
          onChange: (v) => setShowAll(v, ctx),
        },
      ],
      sortBy: "Expires",
      rowClass: (s) => (isLive(s) ? "" : "row-faint"),
      emptyTitle: "No sandboxes yet",
      emptyBody:
        "Start one with Create sandbox, or with " +
        "'keera sandbox create <name> --class <class>'.",
    },
  );
}

/** The organisation's classes. */
function classCard(ctx, classes, driver) {
  const canEdit = isAdmin(ctx);
  return h(
    "div",
    { class: "stack", style: { gap: "10px" } },
    h(
      "h2",
      { style: { marginBottom: "0" } },
      "Classes",
      h("span", { class: "faint" }, ` — the machines ${orgNameOf(ctx)} offers`),
    ),
    canEdit && classes.length
      ? h(
          "div",
          { class: "hint" },
          "To add or change classes, run 'keera sandbox apply <file> --org " +
            ctx.orgID +
            "'.",
        )
      : null,
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
          label: "",
          shrink: true,
          cell: (c) =>
            canEdit
              ? h(
                  "button",
                  {
                    class: "btn btn-sm btn-danger",
                    title: "Delete this class",
                    "aria-label": `Delete ${c.name}`,
                    onClick: () => removeClass(ctx, c),
                  },
                  icon(icons.trash),
                )
              : null,
        },
      ],
      classes,
      {
        emptyTitle: "No sandbox classes yet",
        emptyBody: canEdit
          ? "A new organisation starts with the classes in " +
            "KEERA_SANDBOXES_FILE. To add classes to this one, run " +
            "'keera sandbox apply <file> --org " +
            ctx.orgID +
            "'."
          : "An administrator of this organisation adds them.",
      },
    ),
  );
}

/** delivers reports whether the driver has a runtime for this tier. Having a
 *  stronger one is not enough: each tier needs its own. */
function delivers(driver, isolation) {
  return (driver.tiers || ["standard"]).includes(isolation || "standard");
}

/** isolationCell says both what a class asks for and whether it can be given. */
function isolationCell(c, driver) {
  if (delivers(driver, c.isolation))
    return pill(c.isolation, c.isolation === "vm" ? "good" : "");
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
          "This deployment has no runtime for this isolation tier, so this class cannot start",
      },
      "not available",
    ),
  );
}

/* ------------------------------------------------------------------ actions */

function actions(ctx, s, canAdmin, me, driver) {
  const mine = s.user_id && s.user_id === me.user_id;
  if (!isLive(s) || (!canAdmin && !mine)) return null;
  const home = keepsHome(s, driver);
  const btns = [];
  if (s.state === "suspended" || s.state === "expired") {
    btns.push(
      h(
        "button",
        {
          class: "btn btn-sm",
          title:
            (home ? "Resume it with its files intact" : "Resume it") +
            (s.state === "expired" ? ", a new key and a new lifetime" : ""),
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
          class: "btn btn-sm",
          title: home
            ? "Release its compute and keep its files"
            : "Release its compute. It has no volume, so its files are lost",
          onClick: () =>
            act(ctx, () => api.suspendSandbox(s.id), `${s.name} is suspending`),
        },
        "Suspend",
      ),
    );
  }
  // The server refuses to extend an expired or failed sandbox. An expired one
  // is resumed instead; a failed one can only be terminated.
  if (s.state !== "expired" && s.state !== "failed") {
    btns.push(
      h(
        "button",
        {
          class: "btn btn-sm",
          title: "Extend its expiry",
          onClick: () => extend(ctx, s),
        },
        "Extend",
      ),
    );
  }
  btns.push(
    h(
      "button",
      {
        class: "btn btn-sm btn-danger",
        title: "Terminate it and delete its files",
        "aria-label": `Terminate ${s.name}`,
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
    toast(message, "good");
    ctx.reload();
  } catch (e) {
    toast(e.message, "bad");
  }
}

function extend(ctx, s) {
  const ttl = h("input", {
    class: "input",
    placeholder: "the class's default",
  });
  const err = h("div");
  modal({
    title: `Extend ${s.name}`,
    subtitle:
      "Counted from now, not added to the time left. The same limits as at " +
      "creation apply.",
    body: h(
      "form",
      {},
      err,
      field("Lifetime from now", ttl, "Leave empty for the class default."),
    ),
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
        "Its home directory goes with it. Anything not pushed is lost.",
      ),
      h(
        "div",
        { class: "faint" },
        "Its API key is revoked. Its cost stays in 'keera sandbox usage'.",
      ),
    ),
    confirmLabel: "Terminate",
    danger: true,
    onConfirm: async () => {
      await api.terminateSandbox(s.id);
      toast(`${s.name} terminated`, "good");
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
    confirmLabel: "Delete class",
    danger: true,
    onConfirm: async () => {
      await api.deleteSandboxClass(c.name, c.org_id);
      toast(`${c.name} deleted`, "good");
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
function newSandbox(ctx, classes, limits, projects) {
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
  // Which project the sandbox - and the key minted for it - belongs to.
  //
  // It is the field with the most behind it: the project decides the sandbox's
  // quota, its budget, its rate limit, the system prompt its agent carries and
  // which models that agent may reach. The oldest project is picked, as the
  // control plane does for a sandbox that names none.
  const first = oldestProject(projects);
  const project = h(
    "select",
    { class: "input" },
    ...projects.map((t) =>
      h("option", { value: t.id, selected: t.id === first }, t.name),
    ),
  );
  const repo = h("input", {
    class: "input",
    placeholder: "https://git.example.internal/project/service.git",
  });
  const branch = h("input", { class: "input", placeholder: "main" });
  const repos = (limits && limits.allowed_repos) || [];
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
    title: "Create sandbox",
    subtitle:
      "It gets its own API key, scoped to you and revoked with the sandbox.",
    wide: true,
    body: h(
      "form",
      {},
      err,
      field(
        "Name",
        name,
        "Lowercase letters, digits and hyphens. It becomes the hostname: " +
          "keera sandbox ssh <name>.",
      ),
      field("Class", cls),
      projects.length
        ? field(
            "Project",
            project,
            "The sandbox's key is scoped to this project, and the sandbox " +
              "counts towards its quota. The project's guardrails can allow " +
              "fewer classes or a shorter lifetime than shown here.",
          )
        : null,
      field(
        "Your ssh public key (required)",
        key,
        "The key that can open a shell in it. 'keera sandbox create' sends " +
          "yours from ~/.ssh automatically.",
      ),
      // Nil or empty allows no repository, and a project can only narrow it.
      repos.length
        ? field(
            "Repository",
            repo,
            "Optional. Cloned with a credential made for this sandbox only. " +
              (repos.includes("*")
                ? "Any repository the forge credential reaches."
                : `Allowed: ${repos.join(", ")}.`),
          )
        : null,
      repos.length ? field("Branch", branch) : null,
      field(
        "Lifetime",
        ttl,
        limits && limits.max_sandbox_ttl_seconds
          ? `Capped at ${duration(limits.max_sandbox_ttl_seconds * 1000)} for ${orgNameOf(ctx)}.`
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
              [sandboxNameProblem(name.value.trim()), name],
              [
                keys.length
                  ? ""
                  : "Paste an ssh public key. Without one, nobody can open " +
                    "a shell in this sandbox. 'cat ~/.ssh/id_ed25519.pub' " +
                    "prints yours.",
                key,
              ],
            ]) {
              if (check[0]) {
                showError(err, check[0]);
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
                project_id: project.value,
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
        "Create sandbox",
      ),
    ],
  });
}

/* ------------------------------------------------------------------ helpers */

/** projectName is a project's name where the screen knows it, and its id otherwise.
 *
 *  A member's list is narrowed to their own sandboxes and they cannot read the
 *  project list, so the id is what they get - which is still better than nothing,
 *  because it is the string their administrator will ask them for. */
function projectName(ctx, id) {
  const known = (ctx.state.projects || []).find((t) => t.id === id);
  return known ? known.name : id;
}

// isLive matches Sandbox.Live on the server. A failed engineer sandbox keeps
// its volume, so it still counts, and it still needs a Terminate button.
function isLive(s) {
  return (
    ["pending", "ready", "suspended", "expired"].includes(s.state) ||
    (s.state === "failed" && s.purpose === "engineer")
  );
}

// stateLabel capitalises a state, as every other status pill here is.
function stateLabel(state) {
  return state ? state[0].toUpperCase() + state.slice(1) : "";
}

function stateTone(state) {
  switch (state) {
    case "ready":
      return "good";
    case "pending":
      return "accent";
    case "suspended":
    case "expired":
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

// keepsHome reports whether the home directory survives a suspend. Podman
// always keeps it in a volume. On Kubernetes, only a class with a disk does.
function keepsHome(s, driver) {
  return s.disk_mib > 0 || (driver && driver.name === "podman");
}

function size(s) {
  return sizeOf(s.cpu_millis, s.memory_mib);
}

function sizeOf(cpuMillis, memoryMiB) {
  const cores =
    cpuMillis % 1000 === 0 ? cpuMillis / 1000 : (cpuMillis / 1000).toFixed(1);
  return `${cores} ${plural(cores, "core")}, ${gib(memoryMiB)}`;
}

function gib(mib) {
  if (!mib) return "0";
  return mib % 1024 === 0 ? `${mib / 1024}Gi` : `${mib}Mi`;
}

/** allowed reports whether this reader could actually be given this class, so
 *  the create dialog offers only the ones that would not be refused. */
function allowed(c, limits, driver) {
  if (!delivers(driver, c.isolation)) return false;
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
 *  somebody wants to see finished sandboxes is a fact about how they are
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
