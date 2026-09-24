// The Keera Gateway control panel: boot, shell and routing.

import { api, setCsrf, ApiError } from "./api.js";
import { h, icon, icons, brandMark, clear, replace, toast } from "./ui.js";
import { signIn } from "./views/signin.js";
import { overviewView } from "./views/overview.js";
import { teamsView } from "./views/teams.js";
import { keysView } from "./views/keys.js";
import { modelsView } from "./views/models.js";
import { filtersView, filterDetailView } from "./views/filters.js";
import { routersView, routerDetailView } from "./views/routers.js";
import { sandboxesView } from "./views/sandboxes.js";
import { mcpView } from "./views/mcp.js";
import { playgroundView } from "./views/playground.js";
import { connectView } from "./views/connect.js";
import { accessView } from "./views/access.js";
import { usageView } from "./views/usage.js";
import { auditView } from "./views/audit.js";
import { requestsView } from "./views/requests.js";
import { mapView } from "./views/map.js";
import { sessionsView, sessionDetailView } from "./views/sessions.js";
import { peopleView } from "./views/people.js";
import { orgsView } from "./views/orgs.js";
import {
  teamDetailView,
  keyDetailView,
  modelDetailView,
} from "./views/detail.js";

const root = document.getElementById("root");

/** state is everything the shell and the views share. */
export const state = {
  me: null,
  orgs: [],
  /** setup is what this deployment has: the counts the first-run checklist
   *  reads, and the three that decide whether Filters, Routers and Sandboxes
   *  are on the sidebar at all. */
  setup: {},
  /** orgID is which organisation the panel is looking at. For anyone bound to
   *  one it is theirs; an operator can switch. */
  orgID: "",
};

/** routes is the whole panel, in sidebar order. The list stays flat - routing,
 *  the fallback route and the aria-current sweep all walk this one array - and
 *  `group` folds it into labelled blocks at the point of rendering.
 *
 *  A `detail` on an entry is that screen's row opened up: /teams lists the
 *  teams, /teams/team_1 is one of them. It hangs off the list route so that who
 *  may see it, and which sidebar entry lights up, are decided once for both.
 *
 *  The order is by audience, widest first: the screens shaped for members lead,
 *  then the ones for whoever runs the organisation, then the ones only an
 *  administrator or an operator can see. */
const routes = [
  { path: "/", label: "Overview", icon: "overview", view: overviewView },

  // The screens a developer rather than an administrator comes here for. For a
  // member these are the only ones that answer a question they actually have,
  // so they come before anything that member could only read.
  {
    path: "/access",
    group: "You",
    label: "My access",
    icon: "access",
    view: accessView,
  },
  {
    path: "/connect",
    group: "You",
    label: "Connect a client",
    icon: "connect",
    view: connectView,
  },
  // The playground verifies that the path an editor will take works. That is a
  // question the person about to configure an editor has, not a piece of
  // configuration, so it sits here rather than among the catalogue screens.
  {
    path: "/playground",
    group: "You",
    label: "Playground",
    icon: "playground",
    view: playgroundView,
  },

  // What the organisation is made of, and the report over it. Usage belongs
  // with them: it is the organisation's spend, not the reader's own.
  {
    path: "/teams",
    group: "Organisation",
    label: "Teams",
    icon: "teams",
    view: teamsView,
    detail: { label: "Team", view: teamDetailView },
  },
  {
    path: "/keys",
    group: "Organisation",
    label: "API keys",
    icon: "keys",
    view: keysView,
    detail: { label: "API key", view: keyDetailView },
  },
  {
    path: "/models",
    group: "Organisation",
    label: "Models",
    icon: "models",
    view: modelsView,
    detail: { label: "Model", view: modelDetailView },
  },
  // Filters sit next to the catalogue because that is what they are made of -
  // a model and an instruction - and next to the guardrails they belong to,
  // which are reached from Teams and API keys either side of them.
  {
    path: "/filters",
    group: "Organisation",
    label: "Filters",
    icon: "filters",
    view: filtersView,
    // Folded away until this deployment has one. See advancedHidden: a first
    // deployment has sixteen screens and needs nine, and a filter is not a
    // thing anybody sets up before they have a reason to.
    advanced: "filters",
    about: "Checks requests, and can redact or refuse them.",
    // A filter has its own screen for the same reason a team and a model do:
    // it is a thing with traffic. It is the only guardrail that runs a model on
    // every request it covers, spends money on each one and refuses some of
    // them, and none of that fits in a row.
    detail: { label: "Filter", view: filterDetailView },
  },
  // Routers sit after Filters because they are the same shape of thing - a
  // model and an instruction, run before the request reaches its model - and
  // read most easily as the pair they are. A filter changes the request; a
  // router changes which model answers it.
  {
    path: "/routers",
    group: "Organisation",
    label: "Routers",
    icon: "routers",
    view: routersView,
    advanced: "routers",
    about: "Picks the model for each request.",
    // A router has its own screen because the one number that says whether it
    // works is a split: which destinations it has been choosing, and how
    // often. Every request it places is answered either way, so nothing else
    // in the panel would ever show a router that had stopped deciding.
    detail: { label: "Router", view: routerDetailView },
  },
  // MCP servers sit after the two hooks: like them they are something a
  // request passes through on its way out, and like the models they are a
  // catalogue every tenant shares.
  {
    path: "/mcp",
    group: "Organisation",
    label: "MCP",
    icon: "mcp",
    view: mcpView,
    advanced: "mcp_servers",
    about: "Tools for agents, behind the same keys and guardrails.",
  },
  // Sandboxes sit after the guardrails and before the report: the classes are
  // a catalogue like the models, the quota is a guardrail like the rest, and
  // what the machines cost shows up in the usage screen below.
  //
  // Not under "You", despite being a thing a developer asks for. A member sees
  // their own and an administrator sees all of them, so splitting it in two
  // would have been two screens showing the same table.
  {
    path: "/sandboxes",
    group: "Organisation",
    label: "Sandboxes",
    icon: "sandboxes",
    view: sandboxesView,
    advanced: "sandboxes",
    // Not shown at all without a driver: there is nothing to lend out.
    needs: "sandboxes",
    about: "Short-lived machines for agents and engineers.",
  },
  {
    path: "/usage",
    group: "Organisation",
    label: "Usage",
    icon: "usage",
    view: usageView,
  },

  {
    path: "/users",
    group: "Administration",
    label: "Users",
    icon: "people",
    view: peopleView,
    admin: true,
  },

  // The deployment drawn rather than listed: what is pointed at this gateway,
  // what it forwards to, and which of those are outside the building. It leads
  // the three screens made of the event log because it is the one that answers
  // "what is this" rather than "what happened" - which is the first question
  // somebody who has just been handed a deployment actually has.
  {
    path: "/map",
    group: "Administration",
    label: "Live map",
    icon: "map",
    view: mapView,
    admin: true,
  },

  // The event log grouped into the tasks its requests were made for: one row
  // per task rather than per call. It leads the pair because the task is the
  // unit anybody works in, and the calls behind any row are a click away.
  //
  // Administration rather than Organisation, like the log below it: the rows
  // name other people's keys and carry error text written by the inference
  // plane. A member investigating their own calls has them on My access.
  {
    path: "/sessions",
    group: "Administration",
    label: "Sessions",
    icon: "sessions",
    view: sessionsView,
    admin: true,
    detail: { label: "Session", view: sessionDetailView },
  },
  // The same log, one row per call: served, refused, interrupted and failed.
  // It is what the dashboard's numbers are counted from, and where a reader
  // ends up once they know which task they are asking about.
  {
    path: "/requests",
    group: "Administration",
    label: "Requests",
    icon: "requests",
    view: requestsView,
    admin: true,
  },
  {
    path: "/audit",
    group: "Administration",
    label: "Audit log",
    icon: "audit",
    view: auditView,
    admin: true,
  },

  {
    path: "/organisations",
    group: "Operator",
    label: "Organisations",
    icon: "orgs",
    view: orgsView,
    operator: true,
  },
];

/** navGroups folds the visible routes into the blocks the sidebar draws. A
 *  group whose every entry was filtered out disappears with its label, so an
 *  administrator never sees an empty "Operator" heading. */
function navGroups(items) {
  const groups = [];
  for (const r of items) {
    const last = groups[groups.length - 1];
    if (last && last.group === (r.group || "")) last.items.push(r);
    else groups.push({ group: r.group || "", items: [r] });
  }
  return groups;
}

/** advancedShown is whether the reader has asked to see the screens this
 *  deployment does not use. Remembered, because somebody who opened them once
 *  is somebody who is setting one up. */
function advancedShown() {
  return localStorage.getItem("keera.nav.more") === "shown";
}

/** inUse reports whether the deployment has any of the thing a route is for.
 *  A screen that has something on it is never hidden, whatever the setting:
 *  what is folded away is the empty screen nobody has asked for yet, not the
 *  filter somebody is running. */
function inUse(route) {
  return (state.setup[route.advanced] || 0) > 0;
}

/** offered reports whether this deployment has the feature a route is for at
 *  all. A route that is not offered is never shown, not even under "More". */
function offered(route) {
  return !route.needs || Boolean(state.me[route.needs]);
}

function visibleRoutes() {
  return routes.filter((r) => {
    if (!offered(r)) return false;
    if (r.operator && !state.me.unrestricted) return false;
    if (r.admin && !(state.me.unrestricted || state.me.role === "admin"))
      return false;
    // A link somebody was sent, or a page they bookmarked, opens whatever the
    // sidebar is showing - otherwise matchRoute would fall back to Overview
    // and the address bar would quietly rewrite itself.
    if (r.advanced && !advancedShown() && !inUse(r) && location.pathname !== r.path)
      return false;
    return true;
  });
}

/** foldedAway is the advanced screens the sidebar is currently not showing,
 *  which is what the "More" entry is offering. */
function foldedAway() {
  if (advancedShown()) return [];
  return routes.filter((r) => r.advanced && offered(r) && !inUse(r));
}

/** matchRoute resolves a path to the screen that draws it.
 *
 *  A detail path is the list path plus one segment, and that segment is what
 *  the screen is given: /teams/team_1, /models/keera-code. It is decoded here
 *  rather than by the view, because a model alias is written by an operator and
 *  can contain anything a URL would otherwise read as structure.
 *
 *  Anything that matches nothing falls back to the first screen the reader may
 *  see. */
function matchRoute(available, pathname) {
  const exact = available.find((r) => r.path === pathname);
  if (exact) return { route: exact, param: "" };

  for (const r of available) {
    if (!r.detail || !pathname.startsWith(r.path + "/")) continue;
    const raw = pathname.slice(r.path.length + 1);
    if (!raw || raw.includes("/")) continue;
    return {
      route: { ...r, label: r.detail.label, view: r.detail.view, parent: r },
      param: decode(raw),
    };
  }
  return { route: available[0], param: "" };
}

function decode(segment) {
  try {
    return decodeURIComponent(segment);
  } catch {
    // A path the browser will not decode is not an id anything here issued.
    return segment;
  }
}

export function navigate(path) {
  closeDrawer();
  if (location.pathname !== path) history.pushState({}, "", path);
  renderRoute();
}

window.addEventListener("popstate", () => renderRoute());

/* ---------------------------------------------------------------- theme */

function currentTheme() {
  return localStorage.getItem("keera.theme") || "system";
}

function applyTheme(theme) {
  if (theme === "system")
    document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", theme);
}

function cycleTheme() {
  const order = ["system", "light", "dark"];
  const next = order[(order.indexOf(currentTheme()) + 1) % order.length];
  localStorage.setItem("keera.theme", next);
  applyTheme(next);
  toast(`Theme: ${next}`);
}

/* ---------------------------------------------------------------- shell */

let outlet;
let topbarHost;
let shell;
let collapseButton;

// The sidebar collapses to a strip of icons. The choice is remembered, because
// someone who took the room back wants it back on their next visit too.
function navCollapsed() {
  return localStorage.getItem("keera.nav") === "collapsed";
}

function applyNavCollapsed(collapsed) {
  shell.classList.toggle("nav-collapsed", collapsed);
  const label = collapsed ? "Expand sidebar" : "Collapse sidebar";
  collapseButton.setAttribute("aria-expanded", String(!collapsed));
  collapseButton.setAttribute("aria-label", label);
  collapseButton.title = label;
}

function toggleNav() {
  const collapsed = !navCollapsed();
  localStorage.setItem("keera.nav", collapsed ? "collapsed" : "expanded");
  applyNavCollapsed(collapsed);
}

/* On a narrow screen the sidebar is a drawer instead: fifteen entries do not
 * make a usable row above the page, so they stay the list they are and the
 * hamburger in the topbar slides them in over it.
 *
 * Unlike the collapsed sidebar this is not remembered. A drawer is opened to go
 * somewhere, and arriving is what closes it - by the navigation, the scrim,
 * Escape, or the window becoming wide enough for the sidebar itself. */
let drawerOpen = false;

function setDrawer(open) {
  drawerOpen = open;
  if (shell) shell.classList.toggle("nav-open", open);
  // The button is rebuilt with the topbar on every route, so it is found
  // rather than held.
  const toggle = document.querySelector(".nav-toggle");
  if (toggle) applyDrawerButton(toggle);
}

function applyDrawerButton(toggle) {
  const label = drawerOpen ? "Close menu" : "Menu";
  toggle.setAttribute("aria-expanded", String(drawerOpen));
  toggle.setAttribute("aria-label", label);
  toggle.title = label;
}

function closeDrawer() {
  if (drawerOpen) setDrawer(false);
}

document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closeDrawer();
});

// The same breakpoint the stylesheet uses, so the drawer cannot be left open
// behind a sidebar that is on screen anyway.
matchMedia("(min-width: 991px)").addEventListener("change", (e) => {
  if (e.matches) closeDrawer();
});

/** moreItem reveals the screens this deployment does not use yet, and puts
 *  them away again.
 *
 *  It names them rather than saying "More", because the whole point is that
 *  somebody who does not know what a router is should be able to decide from
 *  the sidebar whether they want one. Once any of them is in use it stops
 *  offering that one, and when all three are it disappears. */
function moreItem() {
  // Nothing to offer once the deployment uses all three: they are on the
  // sidebar because they are in use, and an entry that would put them away
  // again is one nobody wants.
  if (routes.filter((r) => r.advanced && offered(r)).every(inUse)) return null;

  const hidden = foldedAway();
  if (!hidden.length && !advancedShown()) return null;

  const label = hidden.length
    ? hidden.map((r) => r.label).join(", ")
    : "Fewer screens";
  const title = hidden.length
    ? hidden.map((r) => `${r.label} - ${r.about}`).join("\n")
    : "Hide unused screens";

  return h(
    "button",
    {
      class: "nav-item nav-more",
      title,
      onClick: () => {
        localStorage.setItem(
          "keera.nav.more",
          advancedShown() ? "hidden" : "shown",
        );
        renderShell();
        renderRoute();
      },
    },
    icon(advancedShown() ? icons.collapse : icons.plus),
    h("span", { class: "nav-text" }, label),
  );
}

function renderShell() {
  outlet = h("div", { class: "content" });
  topbarHost = h("div");

  collapseButton = h(
    "button",
    { class: "nav-item nav-collapse", onClick: toggleNav },
    icon(icons.collapse),
    h("span", { class: "nav-text" }, "Collapse"),
  );

  const nav = h(
    "nav",
    { class: "sidebar", id: "sidebar" },
    h(
      "a",
      {
        class: "brand",
        href: "/",
        title: "Overview",
        onClick: (e) => {
          e.preventDefault();
          navigate("/");
        },
      },
      brandMark(22),
      h("span", { class: "nav-text" }, "Keera Gateway"),
    ),
    navGroups(visibleRoutes()).map(({ group, items }) =>
      h(
        "div",
        { class: "nav-group" },
        group ? h("div", { class: "nav-label" }, group) : null,
        items.map((r) =>
          h(
            "a",
            {
              class: "nav-item",
              href: r.path,
              // The only label left once the sidebar is collapsed, and on the
              // three folded-away screens the place to say what they are for:
              // somebody who has just revealed "Filters" has by definition not
              // used one.
              title: r.about ? `${r.label} - ${r.about}` : r.label,
              dataset: { path: r.path },
              onClick: (e) => {
                e.preventDefault();
                navigate(r.path);
              },
            },
            icon(icons[r.icon]),
            h("span", { class: "nav-text" }, r.label),
          ),
        ),
        group === "Organisation" ? moreItem() : null,
      ),
    ),
    h(
      "div",
      { class: "sidebar-foot" },
      collapseButton,
      h(
        "div",
        { class: "account" },
        h(
          "div",
          { class: "account-id" },
          h("div", { class: "account-email" }, state.me.email || "Operator key"),
          h("div", { class: "account-role" }, state.me.role),
        ),
        h(
          "button",
          {
            class: "btn btn-quiet btn-sm",
            title: "Sign out",
            "aria-label": "Sign out",
            onClick: signOut,
          },
          icon(icons.signout),
        ),
      ),
    ),
  );

  shell = h(
    "div",
    { class: "shell" },
    nav,
    h("main", { class: "main" }, topbarHost, outlet),
    // Only ever on screen with the drawer, where it both dims the page and
    // gives the tap outside the drawer somewhere to land.
    h("div", {
      class: "nav-scrim",
      "aria-hidden": "true",
      onClick: closeDrawer,
    }),
  );
  replace(root, shell);
  drawerOpen = false;
  applyNavCollapsed(navCollapsed());
  root.classList.remove("boot");
}

// The topbar is rebuilt on every route render rather than once at boot, because
// what it shows can change underneath it: creating the first organisation is
// what makes the switcher exist at all, and a switcher that only appears after
// a browser reload is a switcher the person who needed it never saw.
function renderTopbar() {
  replace(topbarHost, topbar());
}

function topbar() {
  const menu = h(
    "button",
    {
      class: "btn btn-quiet btn-sm nav-toggle",
      "aria-controls": "sidebar",
      onClick: () => setDrawer(!drawerOpen),
    },
    icon(icons.menu),
  );
  applyDrawerButton(menu);

  const bar = h(
    "header",
    { class: "topbar" },
    menu,
    h(
      "div",
      { class: "stack" },
      h("h1", { id: "page-title" }, ""),
      h("div", { class: "page-sub", id: "page-sub" }, ""),
    ),
    h("div", { class: "spacer" }),
  );

  // Only an operator sees more than one organisation, so the switcher only
  // exists for them.
  if (state.me.unrestricted && state.orgs.length > 0) {
    const select = h(
      "select",
      {
        class: "select",
        style: { width: "auto" },
        "aria-label": "Organisation",
        onChange: (e) => {
          state.orgID = e.target.value;
          localStorage.setItem("keera.org", state.orgID);
          renderRoute();
        },
      },
      h("option", { value: "" }, "All organisations"),
      state.orgs.map((o) =>
        h("option", { value: o.id, selected: o.id === state.orgID }, o.name),
      ),
    );
    bar.append(select);
  } else if (state.me.org_name) {
    bar.append(h("span", { class: "pill" }, state.me.org_name));
  }

  bar.append(
    h(
      "button",
      {
        class: "btn btn-quiet btn-sm",
        title: "Switch theme",
        "aria-label": "Switch theme",
        onClick: cycleTheme,
      },
      icon(icons.theme),
    ),
  );
  return bar;
}

function setTitle(title, sub) {
  const t = document.getElementById("page-title");
  const s = document.getElementById("page-sub");
  if (t) t.textContent = title;
  if (s) s.textContent = sub || "";
}

/* --------------------------------------------------------------- routing */

/** teardowns are what the screen currently on the outlet has to undo before
 *  another one replaces it. A view that only builds DOM needs none - the DOM
 *  goes when the outlet is cleared. A view that opened something the DOM does
 *  not own, which so far means the request log's live stream, registers it
 *  here: an EventSource nobody closes keeps its connection, and a browser
 *  allows very few of those per origin. */
let teardowns = [];

function tearDown() {
  const pending = teardowns;
  teardowns = [];
  for (const fn of pending) {
    try {
      fn();
    } catch {
      // A screen that is already gone must not be able to stop the next one
      // from being drawn.
    }
  }
}

async function renderRoute() {
  tearDown();
  const available = visibleRoutes();
  const { route, param } = matchRoute(available, location.pathname);

  // A detail screen lights up the list it belongs to: somebody reading one team
  // is still under Teams, and a sidebar with nothing marked reads as a screen
  // that fell out of the panel.
  const current = (route.parent || route).path;
  for (const el of document.querySelectorAll(".nav-item")) {
    el.toggleAttribute("aria-current", el.dataset.path === current);
    if (el.dataset.path === current) el.setAttribute("aria-current", "page");
  }
  if (!route.parent && location.pathname !== route.path) {
    history.replaceState({}, "", route.path);
  }
  renderTopbar();
  setTitle(route.label, "");
  replace(
    outlet,
    h(
      "div",
      { class: "boot", style: { minHeight: "240px" } },
      h("div", { class: "boot-mark" }),
    ),
  );

  let title = route.label;
  const ctx = {
    state,
    orgID: state.orgID,
    currency: state.me.currency || "",
    reload: renderRoute,
    navigate,
    /** param is the one path segment a detail screen was opened with: the team
     *  id, the key id, the model alias. It is empty on every other screen. */
    param,
    setSubtitle: (s) => setTitle(title, s),
    /** onTeardown registers work to undo when this screen goes away - on a
     *  reload, or on navigation to another one. */
    onTeardown: (fn) => teardowns.push(fn),
    /** setTitle is for a screen whose heading is the thing it is showing rather
     *  than the name of the screen - one team is called by its own name, not
     *  "Team". */
    setTitle: (t, s) => {
      title = t;
      setTitle(t, s);
    },
  };
  try {
    const view = await route.view(ctx);
    replace(outlet, view);
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      return boot(); // the session expired underneath us
    }
    replace(
      outlet,
      h(
        "div",
        { class: "banner banner-bad" },
        err.message || "Something went wrong loading this page.",
      ),
    );
  }
}

/* ------------------------------------------------------------- sign out */

async function signOut() {
  try {
    const res = await api.signOut();
    if (res && res.provider_logout_url) {
      location.href = res.provider_logout_url;
      return;
    }
  } catch {
    // Signing out locally is what matters; a provider that refuses is not a
    // reason to leave the person looking signed in.
  }
  location.href = "/";
}

/* ------------------------------------------------------------------ boot */

async function boot() {
  applyTheme(currentTheme());
  clear(root);
  root.classList.add("boot");
  root.append(h("div", { class: "boot-mark" }));

  let me;
  try {
    me = await api.me();
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      return signIn(root, boot);
    }
    root.classList.remove("boot");
    return replace(
      root,
      h(
        "div",
        { class: "signin" },
        h(
          "div",
          { class: "signin-card" },
          h(
            "div",
            { class: "banner banner-bad" },
            err.message || "The control plane is unreachable.",
          ),
          h("button", { class: "btn", onClick: boot }, "Try again"),
        ),
      ),
    );
  }

  state.me = me;
  setCsrf(me.csrf);
  state.orgID = me.org_id || "";

  if (me.unrestricted) {
    try {
      state.orgs = (await api.orgs()).data || [];
    } catch {
      state.orgs = [];
    }
    const remembered = localStorage.getItem("keera.org") || "";
    if (remembered && state.orgs.some((o) => o.id === remembered))
      state.orgID = remembered;
    else if (!state.orgID && state.orgs.length === 1)
      state.orgID = state.orgs[0].id;
  }

  // What this deployment has. Read before the shell is drawn, because the
  // sidebar is built from it - a nav that appeared and then grew three
  // entries a moment later would be worse than one screen too many.
  //
  // A failure here leaves the counts at zero, which folds the three optional
  // screens away rather than breaking the panel. They are one click back.
  try {
    state.setup = await api.setup(state.orgID);
  } catch {
    state.setup = {};
  }

  renderShell();
  await renderRoute();
}

boot();
