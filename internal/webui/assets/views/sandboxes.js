// Sandboxes: the machines this gateway lends out. A sandbox belongs to a
// person in an organisation, uses its quota, and its agent calls the gateway
// on a key minted for it.
//
// There is deliberately no way in from here. An administrator can see and
// terminate every sandbox, because the quota and the bill are theirs, but
// cannot open a shell. Attaching is `keera sandbox ssh`, for the owner and
// operators.

import { api } from "../api.js";
import {
  h,
  table,
  confirm,
  toast,
  pill,
  stat,
  field,
  duration,
  dateTime,
  ago,
  compact,
  num,
  icons,
  iconButton,
  plural,
  isAdmin,
  sandboxNameProblem,
  listHead,
  sectionHead,
  dash,
  formModal,
  capitalise,
} from "../ui.js";
import { orgNameOf } from "./orgs.js";
import { oldestProject } from "./projects.js";

// showFinished lasts while the screen is open: a reload keeps it and leaving
// resets it, like "Show revoked" on keys.
let showFinished = false;

export async function sandboxesView(ctx) {
  // Navigation changes the path before the old screen is torn down; a reload
  // does not.
  const here = location.pathname;
  ctx.onTeardown(() => {
    if (location.pathname !== here) showFinished = false;
  });

  const me = ctx.state.me;
  const canAdmin = isAdmin(ctx);
  const [list, cat, projects] = await Promise.all([
    api.sandboxes(ctx.orgID, { all: showFinished }),
    api.sandboxClasses(ctx.orgID),
    // Only an administrator may pick a project. A member's sandbox goes in
    // the organisation's oldest project.
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

  const live = sandboxes.filter(isLive);
  const running = live.filter((s) => s.state === "ready").length;
  const coreSeconds = sandboxes.reduce(
    (sum, s) => sum + Math.round((s.running_seconds * s.cpu_millis) / 1000),
    0,
  );

  return h(
    "div",
    { class: "grid" },
    head(ctx, classes, driver, limits, canAdmin, ctx.state.projects),
    h(
      "div",
      { class: "grid grid-4" },
      stat("Live", num(live.length), limitNote(limits, canAdmin)),
      stat(
        "Running now",
        num(running),
        "the rest are starting, suspended, expired or failed",
      ),
      // Core-seconds rather than wall time: two minutes of thirty-two cores is
      // not two minutes of two, and a chargeback uses this number. Wall time
      // is on each row.
      stat(
        "Compute used",
        [
          compact(Math.round(coreSeconds / 60)),
          h("span", { class: "unit" }, "core-min"),
        ],
        "over the sandboxes listed",
      ),
      stat(
        "Isolation",
        driver.isolation,
        `the strongest the ${driver.name} driver offers`,
      ),
    ),
    listCard(ctx, sandboxes, canAdmin, me, driver),
    classCard(ctx, classes, driver),
  );
}

function head(ctx, classes, driver, limits, canAdmin, projects) {
  const usable = classes.filter((c) => allowed(c, limits, driver));
  return listHead(
    [
      "A sandbox is a machine for one task or one working day, with the " +
        "toolchain installed and its own API key. It expires on its own. Its " +
        "owner, or an operator, opens a shell in it with ",
      h("code", {}, "keera sandbox ssh <name>"),
      ".",
    ],
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
            s.repo ? h("div", { class: "faint small" }, s.repo) : null,
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
              pill(capitalise(s.state), stateTone(s.state)),
              s.purpose === "agent" ? pill("Agent", "accent") : null,
            ),
            // The other states explain themselves.
            (s.state === "pending" || s.state === "failed") && s.detail
              ? h("div", { class: "faint small" }, s.detail)
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
            // The machine it was given, not what the class says now: a class
            // can change under a running sandbox.
            h(
              "div",
              { class: "faint small" },
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
            s.owner
              ? h("span", { class: "muted" }, s.owner)
              : h("span", { class: "faint" }, "No user"),
            // Beside the owner, because together they say whose it is.
            s.project_id
              ? h(
                  "div",
                  { class: "faint small" },
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
        // Time remaining rather than a timestamp, because "in 40m" is what
        // somebody acts on.
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
      // Not a filter: finished sandboxes are fetched only on request, because
      // the list is capped and the cap is for the live ones.
      toggles: [
        {
          label: "Show finished",
          on: showFinished,
          onChange: (v) => {
            showFinished = v;
            ctx.reload();
          },
        },
      ],
      sortBy: "Expires",
      rowClass: (s) => (isLive(s) ? "" : "row-faint"),
      emptyTitle: "No sandboxes yet",
      emptyBody: [
        "Start one with Create sandbox, or with ",
        h("code", {}, "keera sandbox create <name> --class <class>"),
        ".",
      ],
    },
  );
}

function classCard(ctx, classes, driver) {
  const canEdit = isAdmin(ctx);
  return h(
    "div",
    { class: "stack", style: { gap: "10px" } },
    sectionHead("Classes", `the machines ${orgNameOf(ctx)} offers`, {
      margin: "0",
    }),
    canEdit && classes.length
      ? h(
          "div",
          { class: "hint" },
          "To add or change classes, run ",
          h("code", {}, `KEERA_ORG=${ctx.orgID} keera sandbox apply <file>`),
          ".",
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
                ? h("div", { class: "faint small" }, c.description)
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
          label: "Disk",
          shrink: true,
          num: true,
          cell: (c) =>
            c.disk_mib ? h("span", { class: "mono" }, gib(c.disk_mib)) : dash(),
        },
        {
          label: "Lifetime",
          shrink: true,
          num: true,
          cell: (c) =>
            h(
              "span",
              { class: "mono" },
              `${duration(c.default_ttl_seconds * 1000)} / ${duration(c.max_ttl_seconds * 1000)}`,
            ),
        },
        {
          label: "Warm",
          shrink: true,
          num: true,
          cell: (c) =>
            c.warm ? h("span", { class: "mono" }, num(c.warm)) : dash(),
        },
        {
          label: "",
          shrink: true,
          cell: (c) =>
            canEdit
              ? iconButton(
                  icons.trash,
                  "Delete this class",
                  () => removeClass(ctx, c),
                  { danger: true, label: `Delete ${c.name}` },
                )
              : null,
        },
      ],
      classes,
      {
        emptyTitle: "No sandbox classes yet",
        emptyBody: canEdit
          ? [
              "A new organisation starts with the classes in " +
                "KEERA_SANDBOXES_FILE. To add classes to this one, run ",
              h(
                "code",
                {},
                `KEERA_ORG=${ctx.orgID} keera sandbox apply <file>`,
              ),
              ".",
            ]
          : "An administrator of this organisation adds them.",
      },
    ),
  );
}

/** delivers reports whether the driver has a runtime for this tier. A
 *  stronger one is not enough: each tier needs its own. */
function delivers(driver, isolation) {
  return (driver.tiers || ["standard"]).includes(isolation || "standard");
}

function isolationCell(c, driver) {
  if (delivers(driver, c.isolation))
    return pill(
      isolationLabel(c.isolation),
      c.isolation === "vm" ? "good" : "",
    );
  // Such a class is refused at creation rather than run at a weaker tier, so
  // say so here, before anyone tries.
  return h(
    "div",
    { class: "row-tight" },
    pill(isolationLabel(c.isolation), "warn"),
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
  } else if (s.state === "ready" && s.purpose !== "agent") {
    // An agent's sandbox runs its task to the end, so the control plane
    // refuses to suspend it.
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
    iconButton(
      icons.trash,
      "Terminate it and delete its files",
      () => terminateSandbox(ctx, s),
      { danger: true, label: `Terminate ${s.name}` },
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

/** lifetimeInput asks for a lifetime in hours. */
function lifetimeInput() {
  return h("input", {
    class: "input",
    type: "number",
    min: "0.1",
    step: "any",
    inputmode: "decimal",
    placeholder: "the class's default",
  });
}

/** lifetime is what a lifetime input asks for, in the seconds the API takes,
 *  or nothing for the class's default. */
function lifetime(input) {
  const hours = input.value.trim();
  return hours ? { ttl_seconds: Math.round(Number(hours) * 3600) } : {};
}

function extend(ctx, s) {
  const ttl = lifetimeInput();
  formModal({
    title: `Extend ${s.name}`,
    subtitle:
      "Counted from now, not added to the time left. The same limits as at " +
      "creation apply.",
    body: h(
      "form",
      {},
      field("Hours from now", ttl, "Leave empty for the class default."),
    ),
    submitLabel: "Extend",
    done: (res) => `${s.name} now expires ${dateTime(res.expires_at)}`,
    ctx,
    submit: () => api.extendSandbox(s.id, lifetime(ttl)),
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
        "Its API key is revoked. Its cost stays in ",
        h("code", {}, "keera sandbox usage"),
        ".",
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
    title: `Delete ${c.name}?`,
    body: h(
      "div",
      {},
      "Running sandboxes keep their image and resources, so nothing " +
        "breaks. New sandboxes can no longer use this class.",
    ),
    confirmLabel: "Delete class",
    danger: true,
    onConfirm: async () => {
      await api.deleteSandboxClass(c.org_id, c.name);
      toast(`${c.name} deleted`, "good");
      ctx.reload();
    },
  });
}

/** newSandbox is the create dialog. The ssh key is required, because a
 *  sandbox with an empty authorized_keys cannot be opened. */
function newSandbox(ctx, classes, limits, projects) {
  const name = h("input", { class: "input", placeholder: "fix-login" });
  const cls = h(
    "select",
    { class: "select" },
    ...classes.map((c) =>
      h(
        "option",
        { value: c.name },
        `${c.name} · ${sizeOf(c.cpu_millis, c.memory_mib)}, ${c.isolation}`,
      ),
    ),
  );
  // The project decides the sandbox's quota, budget, rate limit, system prompt
  // and models. The oldest is picked, as the control plane does by default.
  const first = oldestProject(projects);
  const project = h(
    "select",
    { class: "select" },
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
  const ttl = lifetimeInput();
  const key = h("textarea", {
    class: "input",
    rows: 3,
    placeholder: "ssh-ed25519 AAAA… you@laptop",
  });

  formModal({
    title: "Create sandbox",
    subtitle:
      "It gets its own API key, scoped to you and revoked with the sandbox.",
    wide: true,
    body: h(
      "form",
      {},
      field("Name", name, [
        "Lowercase letters, digits and hyphens. It becomes the hostname: ",
        h("code", {}, "keera sandbox ssh <name>"),
        ".",
      ]),
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
      field("Your SSH public key", key, [
        "The key that can open a shell in it. ",
        h("code", {}, "keera sandbox create"),
        " sends yours from ~/.ssh automatically.",
      ]),
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
        "Lifetime in hours",
        ttl,
        limits && limits.max_sandbox_ttl_seconds
          ? `Capped at ${duration(limits.max_sandbox_ttl_seconds * 1000)} for ${orgNameOf(ctx)}.`
          : "Leave empty for the class default.",
      ),
    ),
    submitLabel: "Create sandbox",
    done: (sb) => `${sb.name} is starting`,
    ctx,
    submit: () => {
      // Checked here too, so the error shows beside the wrong field.
      const keys = key.value
        .split("\n")
        .map((l) => l.trim())
        .filter((l) => l && !l.startsWith("#"));
      for (const [problem, input] of [
        [sandboxNameProblem(name.value.trim()), name],
        [
          keys.length
            ? ""
            : [
                "Paste an SSH public key. Without one, nobody can open " +
                  "a shell in this sandbox. ",
                h("code", {}, "cat ~/.ssh/id_ed25519.pub"),
                " prints yours.",
              ],
          key,
        ],
      ]) {
        if (problem) {
          input.focus();
          throw { message: problem };
        }
      }
      return api.createSandbox({
        org_id: ctx.orgID,
        name: name.value.trim(),
        class: cls.value,
        project_id: project.value,
        repo: repo.value.trim(),
        branch: branch.value.trim(),
        ...lifetime(ttl),
        authorized_keys: keys,
      });
    },
  });
}

/** projectName falls back to the id, because a member cannot read the
 *  project list. */
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

function isolationLabel(isolation) {
  return isolation === "vm" ? "VM" : capitalise(isolation || "standard");
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
  // Colour the last half hour, while somebody can still act on it.
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

function sizeOf(cpuMillis, memoryMiB) {
  const cores =
    cpuMillis % 1000 === 0 ? cpuMillis / 1000 : (cpuMillis / 1000).toFixed(1);
  return `${cores} ${plural(cores, "core")}, ${gib(memoryMiB)}`;
}

function gib(mib) {
  if (!mib) return "0";
  return mib % 1024 === 0 ? `${mib / 1024}Gi` : `${mib}Mi`;
}

/** allowed reports whether this reader could be given this class, so the
 *  create dialog offers only those. */
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
