// Projects - the screen where an enterprise administrator sets guardrails per
// project, which is the reason the panel exists.

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
  rowLink,
  showError,
  isAdmin,
  plural,
  field,
} from "../ui.js";
import {
  openGuardrails,
  ceilingsFor,
  modelsCell,
  summarise,
} from "./guardrails.js";
import { chooseOrg, orgNameOf } from "./orgs.js";

export async function projectsView(ctx) {
  if (!ctx.orgID && ctx.state.me.unrestricted)
    return chooseOrg(ctx, "Projects");

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

  const head = h(
    "div",
    { class: "row", style: { marginBottom: "16px" } },
    h(
      "div",
      { class: "muted" },
      canEdit
        ? "A project's guardrails apply to every key in it. They can narrow what " +
            "the organisation allows, never widen it."
        : "A project's guardrails apply to every key in it. Open a project to see " +
            "them. An administrator sets them.",
    ),
    h("div", { class: "spacer", style: { flex: 1 } }),
    // The organisation's own guardrails are the ceiling every row here is held
    // inside, and its system prompt reaches every request every member makes.
    // A member is subject to both, so both are theirs to read.
    h(
      "button",
      {
        class: "btn",
        title: "Guardrails that apply to every project",
        onClick: () =>
          openGuardrails(ctx, {
            scope: "org",
            id: orgID,
            name: orgName,
            models,
            orgID,
            canEdit,
          }),
      },
      icon(icons.sliders),
      "Organisation guardrails",
    ),
    canEdit
      ? h(
          "button",
          {
            class: "btn btn-primary",
            onClick: () => newProject(ctx),
          },
          icon(icons.plus),
          "New project",
        )
      : null,
  );

  const rows = table(
    [
      {
        // The name opens the project's own screen: what it has been doing, what
        // it spent doing it, and the requests behind both. The button at the end
        // of the row stays what this screen is for, which is the guardrails.
        label: "Name",
        cell: (t) =>
          h(
            "div",
            { class: "stack" },
            rowLink(
              ctx,
              "/projects/" + encodeURIComponent(t.id),
              t.name,
              "This project's traffic, spend and requests",
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
          t.description
            ? h("span", { class: "muted" }, t.description)
            : h("span", { class: "faint" }, "—"),
      },
      { label: "Models", cell: (t) => modelsCell(t.limits) },
      {
        label: "Guardrails",
        cell: (t) => {
          const bits = summarise(t.limits);
          return bits.length
            ? h("span", { class: "muted nowrap" }, bits.join(" · "))
            : h("span", { class: "faint" }, "inherited");
        },
      },
      {
        label: "Budget",
        cell: (t) => {
          if (!t.budget_micros) {
            return h(
              "div",
              { class: "stack" },
              h("span", { class: "faint" }, "no budget"),
              h(
                "span",
                { class: "muted", style: { fontSize: "11.5px" } },
                money(t.spend_micros, ctx.currency) + " this month",
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
                { class: "muted nowrap", style: { fontSize: "11.5px" } },
                `${Math.round(frac * 100)}%`,
              ),
            ),
            h(
              "span",
              { class: "muted", style: { fontSize: "11.5px" } },
              `${money(t.spend_micros, "")} of ${money(t.budget_micros, ctx.currency)} per ${t.period}`,
            ),
          );
        },
      },
      {
        label: "Keys",
        num: true,
        shrink: true,
        cell: (t) => num(t.active_keys),
      },
      {
        label: "",
        shrink: true,
        cell: (t) =>
          h(
            "div",
            { class: "row-tight" },
            h(
              "button",
              {
                class: "btn btn-sm",
                title: canEdit
                  ? "Set this project's guardrails"
                  : "View this project's guardrails",
                onClick: () =>
                  openProject(ctx, t, models, orgID, orgName, canEdit),
              },
              "Guardrails",
            ),
            // Editing and deleting sit behind the guardrails button rather
            // than beside the name, because the name is a link to the
            // project's own screen and a pencil next to it would be read as
            // editing what that screen shows.
            canEdit
              ? h(
                  "button",
                  {
                    class: "btn btn-sm",
                    title: "Edit this project",
                    "aria-label": `Edit ${t.name}`,
                    onClick: () => editProject(ctx, t),
                  },
                  icon(icons.pencil),
                )
              : null,
            canEdit
              ? h(
                  "button",
                  {
                    class: "btn btn-sm btn-danger",
                    title: "Delete this project",
                    "aria-label": `Delete ${t.name}`,
                    onClick: () => deleteProject(ctx, t),
                  },
                  icon(icons.trash),
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
    },
  );

  return h("div", {}, head, rows);
}

// projectFields are the inputs a new project and an edited one share.
function projectFields(project) {
  const name = h("input", {
    class: "input",
    placeholder: "accounting-team",
    value: project ? project.name : "",
    autofocus: true,
    autocomplete: "off",
  });
  const description = h("textarea", {
    class: "input",
    rows: "3",
    placeholder: "Bookkeeping, invoicing and payroll",
  });
  description.value = project ? project.description || "" : "";
  return { name, description };
}

function newProject(ctx) {
  const { name, description } = projectFields(null);
  const err = h("div");
  modal({
    title: "New project",
    subtitle:
      "Usually one per product or group of people, so budgets and reports " +
      "match how the organisation works.",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      err,
      field("Name", name),
      field("Description", description, "Optional. What the project is for."),
    ),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Cancel"),
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: async (e) => {
            if (!name.value.trim()) return name.focus();
            e.target.disabled = true;
            try {
              await api.createProject(
                ctx.orgID || ctx.state.me.org_id,
                name.value.trim(),
                description.value.trim(),
              );
              close();
              toast("Project created", "good");
              ctx.reload();
            } catch (ex) {
              showError(err, ex.message);
              e.target.disabled = false;
            }
          },
        },
        "Create project",
      ),
    ],
  });
}

// editProject changes a project's labels and nothing else. Keys, guardrails,
// spend and every usage row ever written hold the project by its id, so a
// rename is safe in a way renaming things usually is not - which is worth saying
// in the dialog, because an administrator looking at a month of reports has no
// reason to assume it.
function editProject(ctx, project) {
  const { name, description } = projectFields(project);
  const err = h("div");
  modal({
    title: `Edit ${project.name}`,
    subtitle:
      "Keys, guardrails and spend stay with the project. Reports use a new " +
      "name from now on. The old name stays in the audit log.",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      err,
      field("Name", name),
      field("Description", description, "Optional. What the project is for."),
    ),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Cancel"),
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: async (e) => {
            const next = name.value.trim();
            if (!next) return name.focus();
            const change = {};
            if (next !== project.name) change.name = next;
            const text = description.value.trim();
            if (text !== (project.description || "")) change.description = text;
            if (!Object.keys(change).length) return close();
            e.target.disabled = true;
            try {
              await api.updateProject(project.id, change);
              close();
              toast("Project saved", "good");
              ctx.reload();
            } catch (ex) {
              showError(err, ex.message);
              e.target.disabled = false;
            }
          },
        },
        "Save project",
      ),
    ],
  });
}

// deleteProject removes a project once nothing live is bound to it.
//
// The control plane refuses while any key in the project is not revoked, expired
// ones included, and the row already knows how many that is - so such a project
// is not offered the button at all. Being told why, next to the screen that
// fixes it, is more use than a red button that comes back with the same
// sentence as an error.
//
// Where it can go ahead, the guardrails are the part worth naming: they are the
// only thing in a project that is not recoverable from somewhere else.
function deleteProject(ctx, project) {
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
          "Show keys",
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
      ctx.reload();
    },
  });
}

// openProject opens the shared guardrails dialog with the organisation's limits
// loaded as the ceiling, so the sentence "projects can only narrow" arrives as the
// actual numbers rather than as a warning.
//
// A member gets the same dialog with nothing to fill in. What a project allows and
// what it says to the model on their behalf is what they are working inside;
// the numbers are the answer to "why was I refused", and refusing to show them
// only turns that into a message to an administrator.
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

/** oldestProject is the id of the project a key or sandbox that names none
 *  goes in: the organisation's oldest. */
export function oldestProject(projects) {
  let first = null;
  for (const t of projects) {
    // Ties go by id, as the control plane orders them.
    const d = first && Date.parse(t.created_at) - Date.parse(first.created_at);
    if (!first || d < 0 || (d === 0 && t.id < first.id)) first = t;
  }
  return first ? first.id : "";
}
