// Teams - the screen where an enterprise administrator sets guardrails per
// department, which is the reason the panel exists.

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
  go,
  showError,
} from "../ui.js";
import {
  openGuardrails,
  ceilingsFor,
  modelsCell,
  summarise,
} from "./guardrails.js";
import { chooseOrg } from "./orgs.js";

export async function teamsView(ctx) {
  const [teamsRes, modelsRes] = await Promise.all([
    api.teams(ctx.orgID),
    api.models(),
  ]);
  const teams = teamsRes.data || [];
  const models = (modelsRes.data || []).filter((m) => m.enabled);
  const canEdit = ctx.state.me.unrestricted || ctx.state.me.role === "admin";

  ctx.setSubtitle(`${teams.length} team${teams.length === 1 ? "" : "s"}`);

  if (!ctx.orgID && ctx.state.me.unrestricted) return chooseOrg(ctx, "Teams");

  const orgID = ctx.orgID || ctx.state.me.org_id;
  const orgName =
    ctx.state.me.org_name ||
    (ctx.state.orgs.find((o) => o.id === orgID) || {}).name ||
    "this organisation";

  const head = h(
    "div",
    { class: "row", style: { marginBottom: "16px" } },
    h(
      "div",
      { class: "muted" },
      canEdit
        ? "A team's guardrails apply to every key in it. They can narrow what " +
            "the organisation allows, never widen it."
        : "A team's guardrails apply to every key in it. Open a team to see " +
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
        title: "Limits that apply to every team",
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
            onClick: () => newTeam(ctx),
          },
          icon(icons.plus),
          "New team",
        )
      : null,
  );

  const rows = table(
    [
      {
        // The name opens the team's own screen: what it has been doing, what it
        // spent doing it, and the requests behind both. The button at the end of
        // the row stays what this screen is for, which is the guardrails.
        label: "Name",
        cell: (t) =>
          h(
            "div",
            { class: "stack" },
            rowLink(
              ctx,
              "/teams/" + encodeURIComponent(t.id),
              t.name,
              "This team's traffic, spend and requests",
            ),
            h(
              "span",
              { class: "faint mono", style: { fontSize: "11px" } },
              t.id,
            ),
          ),
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
                  ? "Set this team's guardrails"
                  : "View this team's guardrails",
                onClick: () =>
                  openTeam(ctx, t, models, orgID, orgName, canEdit),
              },
              "Guardrails",
            ),
            // Renaming and deleting sit behind the guardrails button rather
            // than beside the name, because the name is a link to the team's
            // own screen and a pencil next to it would be read as editing what
            // that screen shows.
            canEdit
              ? h(
                  "button",
                  {
                    class: "btn btn-sm",
                    title: "Rename this team",
                    "aria-label": `Rename ${t.name}`,
                    onClick: () => renameTeam(ctx, t),
                  },
                  icon(icons.pencil),
                )
              : null,
            canEdit
              ? h(
                  "button",
                  {
                    class: "btn btn-sm btn-danger",
                    title: "Delete this team",
                    "aria-label": `Delete ${t.name}`,
                    onClick: () => deleteTeam(ctx, t),
                  },
                  icon(icons.trash),
                )
              : null,
          ),
      },
    ],
    teams,
    {
      emptyTitle: "No teams yet",
      emptyBody: "Create one per department, then set its guardrails.",
    },
  );

  return h("div", {}, head, rows);
}

function newTeam(ctx) {
  const name = h("input", {
    class: "input",
    placeholder: "Payments Platform",
    autofocus: true,
  });
  const err = h("div");
  modal({
    title: "New team",
    subtitle:
      "Usually one per department, so budgets and reports match how the " +
      "organisation works.",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      err,
      h("div", { class: "field" }, h("label", {}, "Name"), name),
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
              await api.createTeam(
                ctx.orgID || ctx.state.me.org_id,
                name.value.trim(),
              );
              close();
              toast("Team created", "good");
              ctx.reload();
            } catch (ex) {
              showError(err, ex.message);
              e.target.disabled = false;
            }
          },
        },
        "Create team",
      ),
    ],
  });
}

// renameTeam changes a team's label and nothing else. Keys, guardrails, spend
// and every usage row ever written hold the team by its id, so this is safe in
// a way renaming things usually is not - which is worth saying in the dialog,
// because an administrator looking at a month of reports has no reason to
// assume it.
function renameTeam(ctx, team) {
  const name = h("input", {
    class: "input",
    value: team.name,
    autofocus: true,
    autocomplete: "off",
  });
  const err = h("div");
  modal({
    title: `Rename ${team.name}`,
    subtitle:
      "Keys, guardrails and spend stay with the team. Reports use the new " +
      "name from now on. The old name stays in the audit log.",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      err,
      h("div", { class: "field" }, h("label", {}, "Name"), name),
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
            if (next === team.name) return close();
            e.target.disabled = true;
            try {
              await api.renameTeam(team.id, next);
              close();
              toast(`Renamed to ${next}`, "good");
              ctx.reload();
            } catch (ex) {
              showError(err, ex.message);
              e.target.disabled = false;
            }
          },
        },
        "Rename team",
      ),
    ],
  });
}

// deleteTeam removes a team once nothing live is bound to it.
//
// The control plane refuses while any key in the team still works, and the row
// already knows how many that is - so a team with live keys is not offered the
// button at all. Being told why, next to the screen that fixes it, is more use
// than a red button that comes back with the same sentence as an error.
//
// Where it can go ahead, the guardrails are the part worth naming: they are the
// only thing in a team that is not recoverable from somewhere else.
function deleteTeam(ctx, team) {
  const live = team.active_keys || 0;
  if (live) {
    const close = modal({
      title: `Delete ${team.name}?`,
      body: h(
        "div",
        { class: "muted" },
        `${live} key${live === 1 ? "" : "s"} in this team ${live === 1 ? "is" : "are"} ` +
          "still active. Deleting the team would take " +
          `${live === 1 ? "it" : "them"} with it, and clients using ` +
          `${live === 1 ? "it" : "them"} would be refused. Revoke ` +
          `${live === 1 ? "it" : "them"} first.`,
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
    title: `Delete ${team.name}?`,
    body:
      "Its guardrails and budget counters are deleted. Revoked keys, usage " +
      "history and the audit log are kept for reports.",
    confirmLabel: "Delete team",
    danger: true,
    onConfirm: async () => {
      await api.deleteTeam(team.id);
      toast(`${team.name} deleted`, "good");
      ctx.reload();
    },
  });
}

// openTeam opens the shared guardrails dialog with the organisation's limits
// loaded as the ceiling, so the sentence "teams can only narrow" arrives as the
// actual numbers rather than as a warning.
//
// A member gets the same dialog with nothing to fill in. What a team allows and
// what it says to the model on their behalf is what they are working inside;
// the numbers are the answer to "why was I refused", and refusing to show them
// only turns that into a message to an administrator.
async function openTeam(ctx, team, models, orgID, orgName, canEdit) {
  const ceilings = await ceilingsFor("team", { orgID, orgName });
  await openGuardrails(ctx, {
    scope: "team",
    id: team.id,
    name: team.name,
    models,
    orgID,
    ceilings,
    canEdit,
  });
}
