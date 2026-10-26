// Projects: where an administrator sets guardrails per project.

import { api } from "../api.js";
import {
  h,
  table,
  modal,
  confirm,
  toast,
  money,
  num,
  meter,
  icon,
  icons,
  iconButton,
  rowLink,
  isAdmin,
  plural,
  field,
  listHead,
  pct,
  dash,
  formModal,
  busyButton,
} from "../ui.js";
import {
  openGuardrails,
  ceilingsFor,
  modelsCell,
  summarise,
} from "./guardrails.js";
import { orgNameOf } from "./orgs.js";

export async function projectsView(ctx) {
  const [projectsRes, modelsRes] = await Promise.all([
    api.projects(ctx.orgID),
    api.models(ctx.orgID),
  ]);
  const projects = projectsRes.data || [];
  const models = (modelsRes.data || []).filter((m) => m.enabled);
  const canEdit = isAdmin(ctx);

  ctx.setSubtitle(`${projects.length} ${plural(projects.length, "project")}`);

  const orgID = ctx.orgID || ctx.state.me.org_id;
  const orgName = orgNameOf(ctx, orgID);

  const head = listHead(
    canEdit
      ? "A project's guardrails apply to every key in it. They can narrow what " +
          "the organisation allows, never widen it."
      : "A project's guardrails apply to every key in it. Open a project to see " +
          "them. An administrator sets them.",
    // The organisation's guardrails cap every project and its prompt reaches
    // every request, so a member may read them.
    busyButton(
      { title: "Guardrails that apply to every project" },
      [icon(icons.sliders), "Organisation guardrails"],
      () =>
        openGuardrails(ctx, {
          scope: "org",
          id: orgID,
          name: orgName,
          models,
          orgID,
          canEdit,
        }),
    ),
    canEdit
      ? { label: "Create project", onClick: () => newProject(ctx) }
      : null,
  );

  const rows = table(
    [
      {
        // The name opens the project's screen; the row's button opens the
        // guardrails.
        label: "Name",
        sortKey: (t) => t.name,
        cell: (t) =>
          h(
            "div",
            { class: "stack" },
            rowLink(
              ctx,
              "/projects/" + encodeURIComponent(t.id),
              t.name,
              "This project's traffic, cost and requests",
            ),
            h(
              "span",
              { class: "faint mono", style: { fontSize: "11px" } },
              t.id,
            ),
          ),
      },
      {
        label: "Description",
        cell: (t) =>
          t.description ? h("span", { class: "muted" }, t.description) : dash(),
      },
      { label: "Models", cell: (t) => modelsCell(t.limits) },
      {
        label: "Guardrails",
        cell: (t) => {
          const bits = summarise(t.limits);
          return bits.length
            ? h("span", { class: "muted nowrap" }, bits.join(" · "))
            : h("span", { class: "faint" }, "Inherited");
        },
      },
      {
        label: "Budget",
        // So the ones about to run out come first.
        sortKey: (t) =>
          t.budget_micros ? t.spend_micros / t.budget_micros : null,
        sortDir: "desc",
        cell: (t) => {
          if (!t.budget_micros) {
            return h(
              "div",
              { class: "stack" },
              h("span", { class: "faint" }, "no budget"),
              h(
                "span",
                { class: "muted small" },
                money(t.spend_micros) + " this month",
              ),
            );
          }
          const frac = t.spend_micros / t.budget_micros;
          const tone = frac >= 1 ? "bad" : frac >= 0.8 ? "warn" : "";
          return h(
            "div",
            { class: "stack" },
            h(
              "div",
              { class: "row-tight" },
              meter(frac, tone),
              h(
                "span",
                { class: "muted nowrap small" },
                pct(t.spend_micros, t.budget_micros),
              ),
            ),
            h(
              "span",
              { class: "muted small" },
              `${money(t.spend_micros)} of ${money(t.budget_micros)} per ${t.period}`,
            ),
          );
        },
      },
      {
        label: "Keys",
        num: true,
        shrink: true,
        sortKey: (t) => t.active_keys || 0,
        sortDir: "desc",
        cell: (t) => num(t.active_keys),
      },
      {
        label: "",
        shrink: true,
        cell: (t) =>
          h(
            "div",
            { class: "row-tight" },
            busyButton(
              {
                class: "btn btn-sm",
                title: canEdit
                  ? "Set this project's guardrails"
                  : "View this project's guardrails",
              },
              "Guardrails",
              () => openProject(ctx, t, models, orgID, orgName, canEdit),
            ),
            // Not beside the name, which links to the project's screen.
            canEdit
              ? iconButton(
                  icons.pencil,
                  "Edit this project",
                  () => editProject(ctx, t),
                  { label: `Edit ${t.name}` },
                )
              : null,
            canEdit
              ? iconButton(
                  icons.trash,
                  "Delete this project",
                  () => deleteProject(ctx, t),
                  { danger: true, label: `Delete ${t.name}` },
                )
              : null,
          ),
      },
    ],
    projects,
    {
      emptyTitle: "No projects yet",
      emptyBody:
        "Create one per product or group of people, then set its guardrails.",
      search: (t) => [t.name, t.id, t.description || ""].join(" "),
      searchLabel: "projects",
      ctx,
      open: (t) => "/projects/" + encodeURIComponent(t.id),
    },
  );

  return h("div", {}, head, rows);
}

function projectFields(project) {
  const name = h("input", {
    class: "input",
    placeholder: "Payments Platform",
    value: project ? project.name : "",
    autofocus: true,
    autocomplete: "off",
  });
  const description = h("textarea", {
    class: "input",
    rows: "3",
    placeholder: "Checkout, billing and refunds",
  });
  description.value = project ? project.description || "" : "";
  return { name, description };
}

function newProject(ctx) {
  const { name, description } = projectFields(null);
  formModal({
    title: "Create project",
    subtitle:
      "Usually one per product or group of people, so budgets and reports " +
      "match how the organisation works.",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      field("Name", name),
      field("Description", description, "Optional. What the project is for."),
    ),
    submitLabel: "Create project",
    done: "Project created",
    ctx,
    submit: async () => {
      if (!name.value.trim()) {
        name.focus();
        return false;
      }
      await api.createProject(
        ctx.orgID || ctx.state.me.org_id,
        name.value.trim(),
        description.value.trim(),
      );
    },
  });
}

// editProject changes a project's labels only. Everything refers to the
// project by id, so a rename is safe.
export function editProject(ctx, project) {
  const { name, description } = projectFields(project);
  formModal({
    title: `Edit ${project.name}`,
    subtitle:
      "Keys, guardrails and costs stay with the project. Reports use a new " +
      "name from now on. The old name stays in the audit log.",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      field("Name", name),
      field("Description", description, "Optional. What the project is for."),
    ),
    done: "Project saved",
    ctx,
    submit: async (close) => {
      const next = name.value.trim();
      if (!next) {
        name.focus();
        return false;
      }
      const change = {};
      if (next !== project.name) change.name = next;
      const text = description.value.trim();
      if (text !== (project.description || "")) change.description = text;
      if (!Object.keys(change).length) {
        close();
        return false;
      }
      await api.updateProject(project.id, change);
    },
  });
}

// deleteProject removes a project once nothing live is bound to it. The
// control plane refuses while any key in it is not revoked, so the button
// explains and links to the keys.
export function deleteProject(ctx, project, done = () => ctx.reload()) {
  const live = project.active_keys || 0;
  if (live) {
    const them = plural(live, "it", "them");
    const close = modal({
      title: `Delete ${project.name}?`,
      body: h(
        "div",
        { class: "muted" },
        `${live} ${plural(live, "key")} in this project ${plural(live, "is", "are")} ` +
          "not revoked. Deleting the project would take " +
          `${them} with it, and clients using ${them} would be refused. ` +
          `Revoke ${them} first.`,
      ),
      actions: (dismiss) => [
        h("button", { class: "btn", onClick: dismiss }, "Cancel"),
        h(
          "button",
          {
            class: "btn btn-primary",
            onClick: () => {
              close();
              ctx.navigate("/keys");
            },
          },
          "Go to API keys",
        ),
      ],
    });
    return;
  }
  confirm({
    title: `Delete ${project.name}?`,
    body:
      "Its guardrails and budget counters are deleted. Its revoked keys " +
      "stay, without a project. Usage history and the audit log are kept.",
    confirmLabel: "Delete project",
    danger: true,
    onConfirm: async () => {
      await api.deleteProject(project.id);
      toast(`${project.name} deleted`, "good");
      done();
    },
  });
}

// openProject opens the guardrails dialog with the organisation's limits as
// the ceiling. A member gets it read-only: it answers "why was I refused".
export async function openProject(
  ctx,
  project,
  models,
  orgID,
  orgName,
  canEdit,
) {
  const ceilings = await ceilingsFor("project", { orgID, orgName });
  await openGuardrails(ctx, {
    scope: "project",
    id: project.id,
    name: project.name,
    models,
    orgID,
    ceilings,
    canEdit,
  });
}

/** oldestProject is where a key or sandbox that names none goes. */
export function oldestProject(projects) {
  let first = null;
  for (const t of projects) {
    // Ties go by id, as the control plane orders them.
    const d = first && Date.parse(t.created_at) - Date.parse(first.created_at);
    if (!first || d < 0 || (d === 0 && t.id < first.id)) first = t;
  }
  return first ? first.id : "";
}
