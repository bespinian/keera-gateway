// The Keera Gateway control panel: boot, shell and routing.

import { api, setCsrf, ApiError } from "./api.js";
import {
  h,
  icon,
  icons,
  iconButton,
  brandMark,
  replace,
  toast,
  isAdmin,
  remembered,
  remember,
  banner,
  capitalise,
  personName,
  initials,
  menu,
  docsURL,
} from "./ui.js";
import {
  signIn,
  passkeySignInFlow,
  passwordSignInFlow,
} from "./views/signin.js";
import { signUp, signUpFlow } from "./views/signup.js";
import { passkeySetupView, managePasskeys } from "./views/passkeys.js";
import { dashboardView } from "./views/dashboard.js";
import { firstRunDue, memberRunDue } from "./views/firstrun.js";
import { projectsView } from "./views/projects.js";
import { keysView } from "./views/keys.js";
import { modelsView, catalogueView } from "./views/models.js";
import { filtersView, filterDetailView } from "./views/filters.js";
import { routersView, routerDetailView } from "./views/routers.js";
import { sandboxesView } from "./views/sandboxes.js";
import { mcpView } from "./views/mcp.js";
import { playgroundView } from "./views/playground.js";
import { clientsView } from "./views/connect.js";
import { accessView } from "./views/access.js";
import { usageView } from "./views/usage.js";
import { auditView } from "./views/audit.js";
import { requestsView } from "./views/requests.js";
import { mapView } from "./views/map.js";
import { sessionsView, sessionDetailView } from "./views/sessions.js";
import { peopleView } from "./views/people.js";
import { orgsView, chooseOrg } from "./views/orgs.js";
import { billingView } from "./views/billing.js";
import {
  projectDetailView,
  keyDetailView,
  modelDetailView,
  userDetailView,
} from "./views/detail.js";

const root = document.getElementById("root");

const state = {
  me: null,
  orgs: [],
  /** setup holds the organisation's counts. They drive the first-run
   *  checklist and whether Filters, Routers, MCP and Sandboxes are shown. */
  setup: {},
  /** access is a member's own keys, which decide whether their guide is
   *  due. Null for an administrator. */
  access: null,
  /** orgID is the organisation the panel shows. An operator can switch it. */
  orgID: "",
};

/** routes is the whole panel in sidebar order, widest audience first. It
 *  stays flat so routing walks one array; `group` splits it for the sidebar.
 *  `perOrg` names a screen's contents when an operator has to pick an
 *  organisation first. A `detail` is one row opened up, such as
 *  /projects/project_1; it follows its list for who may see it. */
const routes = [
  { path: "/", label: "Dashboard", icon: "dashboard", view: dashboardView },

  // A developer's screens come first: for a member they are the only ones
  // that matter.
  {
    path: "/access",
    group: "You",
    label: "My access",
    icon: "access",
    view: accessView,
  },
  {
    path: "/clients",
    group: "You",
    label: "My clients",
    icon: "connect",
    view: clientsView,
  },
  // The playground tests the path an editor takes, so it sits with the
  // developer's screens.
  {
    path: "/playground",
    group: "You",
    label: "Playground",
    icon: "playground",
    view: playgroundView,
  },

  // Usage is here because it is the organisation's spend, not the reader's.
  {
    path: "/projects",
    group: "Organisation",
    label: "Projects",
    icon: "projects",
    view: projectsView,
    perOrg: "Projects",
    detail: { label: "Project", view: projectDetailView },
  },
  {
    path: "/keys",
    group: "Organisation",
    label: "API keys",
    icon: "keys",
    view: keysView,
    perOrg: "API keys",
    detail: { label: "API key", view: keyDetailView },
  },
  {
    path: "/models",
    group: "Organisation",
    label: "Models",
    icon: "models",
    view: modelsView,
    perOrg: "Models",
    detail: { label: "Model", view: modelDetailView, perOrg: "Models" },
  },
  // Every model the hosted providers offer. It opens from Models, not the
  // sidebar, so `under` lights up Models.
  {
    path: "/catalogue",
    group: "Organisation",
    label: "Model catalogue",
    icon: "models",
    view: catalogueView,
    under: "/models",
  },
  // What clients call besides models, so it follows them. Filters are among
  // its building blocks, so they come after it.
  {
    path: "/routers",
    group: "Organisation",
    label: "Routers",
    icon: "route",
    view: routersView,
    perOrg: "Routers",
    advanced: "routers",
    about: "What clients call: filters, route steps and limits in order.",
    // Its own screen: the editor does not fit in a dialog.
    detail: {
      label: "Router",
      view: routerDetailView,
      perOrg: "Routers",
    },
  },
  {
    path: "/filters",
    group: "Organisation",
    label: "Filters",
    icon: "filters",
    view: filtersView,
    perOrg: "Filters",
    // Folded away until this deployment has one. See advancedShown.
    advanced: "filters",
    about: "Checks requests. Guardrails and routers use them.",
    // Its own screen: what it did to the traffic does not fit in a row.
    detail: { label: "Filter", view: filterDetailView, perOrg: "Filters" },
  },
  // Like filters and routers, something a request passes through.
  {
    path: "/mcp",
    group: "Organisation",
    label: "MCP",
    icon: "mcp",
    view: mcpView,
    perOrg: "MCP servers",
    advanced: "mcp_servers",
    about: "Tools for agents, behind the same keys and guardrails.",
  },
  // Not under "You": a member sees their own sandboxes and an administrator
  // all of them, in the same table.
  {
    path: "/sandboxes",
    group: "Organisation",
    label: "Sandboxes",
    icon: "sandboxes",
    view: sandboxesView,
    perOrg: "Sandboxes",
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

  // The event log, read three ways. An administrator sees the organisation's
  // traffic, a member only their own keys'. The map leads, as it shows what
  // the deployment is.
  {
    path: "/map",
    group: "Activity",
    label: "Live map",
    icon: "map",
    view: mapView,
    perOrg: "Live maps",
  },
  // The log grouped by task, one row each.
  {
    path: "/sessions",
    group: "Activity",
    label: "Sessions",
    icon: "sessions",
    view: sessionsView,
    detail: { label: "Session", view: sessionDetailView },
  },
  // The same log, one row per call: served, refused, interrupted and failed.
  {
    path: "/requests",
    group: "Activity",
    label: "Requests",
    icon: "requests",
    view: requestsView,
  },

  {
    path: "/users",
    group: "Administration",
    label: "Users",
    icon: "people",
    view: peopleView,
    perOrg: "Users",
    admin: true,
    detail: { label: "User", view: userDetailView },
  },
  {
    path: "/audit",
    group: "Administration",
    label: "Audit log",
    icon: "audit",
    view: auditView,
    admin: true,
  },
  // The bill for the deployment's own provider keys. Not shown without one.
  {
    path: "/billing",
    group: "Administration",
    label: "Billing",
    icon: "billing",
    view: billingView,
    admin: true,
    needs: "billing",
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

/** navGroups folds the visible routes into sidebar blocks, so a group with
 *  nothing visible leaves no empty heading. */
function navGroups(items) {
  const groups = [];
  for (const r of items) {
    const last = groups[groups.length - 1];
    if (last && last.group === (r.group || "")) last.items.push(r);
    else groups.push({ group: r.group || "", items: [r] });
  }
  return groups;
}

/** advancedShown is whether the reader asked to see unused screens.
 *  Remembered, because someone who opened them once is setting one up. */
function advancedShown() {
  return remembered("keera.nav.more", { lasting: true }) === "shown";
}

/** inUse reports whether the deployment has any of what a route is for. A
 *  screen with something on it is never folded away, whatever the setting. */
function inUse(route) {
  return (state.setup[route.advanced] || 0) > 0;
}

/** offered reports whether this deployment has the feature a route is for. A
 *  route that is not offered is never shown, not even by moreItem. */
function offered(route) {
  return !route.needs || Boolean(state.me[route.needs]);
}

function visibleRoutes() {
  return routes.filter((r) => {
    if (!offered(r)) return false;
    if (r.operator && !state.me.unrestricted) return false;
    if (r.admin && !state.me.can_admin_org) return false;
    // A link or bookmark still opens a folded-away screen or one of its rows.
    // Otherwise matchRoute would fall back to Dashboard and rewrite the URL.
    if (
      r.advanced &&
      !advancedShown() &&
      !inUse(r) &&
      location.pathname !== r.path &&
      !location.pathname.startsWith(r.path + "/")
    )
      return false;
    return true;
  });
}

/** foldedAway is the advanced screens the sidebar hides, which moreItem
 *  offers by name. */
function foldedAway() {
  if (advancedShown()) return [];
  return routes.filter((r) => r.advanced && offered(r) && !inUse(r));
}

/** matchRoute finds the screen for a path, or falls back to the first one
 *  the reader may see. A detail path is the list path plus one segment. It is
 *  decoded here, not by the view, so it cannot be read as more of the path. */
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

function navigate(path) {
  closeDrawer();
  if (location.pathname !== path) history.pushState({}, "", path);
  renderRoute();
}

window.addEventListener("popstate", () => renderRoute());

function currentTheme() {
  return remembered("keera.theme", { lasting: true }) || "system";
}

function applyTheme(theme) {
  if (theme === "system")
    document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", theme);
}

function cycleTheme() {
  const order = ["system", "light", "dark"];
  const next = order[(order.indexOf(currentTheme()) + 1) % order.length];
  remember("keera.theme", next, { lasting: true });
  applyTheme(next);
  toast(`Theme: ${capitalise(next)}`);
}

let outlet;
let topbarHost;
let shell;
let collapseButton;

function navCollapsed() {
  return remembered("keera.nav", { lasting: true }) === "collapsed";
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
  remember("keera.nav", collapsed ? "collapsed" : "expanded", {
    lasting: true,
  });
  applyNavCollapsed(collapsed);
}

/* On a narrow screen the sidebar is a drawer the topbar's menu button slides
 * in. It is not remembered: it is opened to go somewhere, so navigating, the
 * scrim, Escape and a wide enough window all close it. */
let drawerOpen = false;

function setDrawer(open) {
  drawerOpen = open;
  if (shell) shell.classList.toggle("nav-open", open);
  // The button is rebuilt with the topbar on every route, so look it up.
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

/** moreItem shows or hides the screens this deployment does not use yet.
 *  It names them rather than saying "More", so someone new can tell whether
 *  they want one. It disappears once all are in use. */
function moreItem() {
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
        remember("keera.nav.more", advancedShown() ? "hidden" : "shown", {
          lasting: true,
        });
        renderShell();
        renderRoute();
      },
    },
    icon(advancedShown() ? icons.collapse : icons.plus),
    h("span", { class: "nav-text" }, label),
  );
}

function navItem(r) {
  return h(
    "a",
    {
      class: "nav-item" + (r.highlight ? " nav-highlight" : ""),
      href: r.path,
      // The only label once the sidebar is collapsed, and where a screen
      // the reader has not used yet says what it is for.
      title: r.about ? `${r.label} - ${r.about}` : r.label,
      dataset: { path: r.path },
      onClick: (e) => {
        e.preventDefault();
        navigate(r.path);
      },
    },
    icon(icons[r.icon]),
    h("span", { class: "nav-text" }, r.label),
  );
}

/** markHome names the first screen after what it shows: the first-run
 *  checklist until the organisation is set up, then the dashboard. The
 *  checklist is highlighted in the sidebar, so it is not missed. */
function markHome() {
  const home = routes[0];
  const ctx = { state, orgID: state.orgID };
  const due = firstRunDue(ctx, state.setup) || memberRunDue(ctx, state.access);
  home.label = due ? "Getting started" : "Dashboard";
  home.icon = due ? "start" : "dashboard";
  home.highlight = due;
  const el = document.querySelector('.nav-item[data-path="/"]');
  if (!el) return;
  const next = navItem(home);
  if (el.hasAttribute("aria-current"))
    next.setAttribute("aria-current", "page");
  el.replaceWith(next);
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
        title: "Dashboard",
        onClick: (e) => {
          e.preventDefault();
          navigate("/");
        },
      },
      brandMark(22),
      h("span", { class: "nav-text" }, "Keera Gateway"),
    ),
    navGroups(visibleRoutes().filter((r) => !r.under)).map(({ group, items }) =>
      h(
        "div",
        { class: "nav-group" },
        group ? h("div", { class: "nav-label" }, group) : null,
        items.map(navItem),
        group === "Organisation" ? moreItem() : null,
      ),
    ),
    h(
      "div",
      { class: "sidebar-foot" },
      collapseButton,
      h(
        "button",
        {
          class: "account",
          type: "button",
          title: state.me.email || "Operator key",
          "aria-haspopup": "menu",
          onClick: (e) => accountMenu(e.currentTarget),
        },
        h(
          "span",
          { class: "avatar", "aria-hidden": "true" },
          state.me.email
            ? initials(state.me.name, state.me.email)
            : icon(icons.keys),
        ),
        h(
          "span",
          { class: "account-id nav-text" },
          h(
            "span",
            { class: "account-name" },
            state.me.email ? personName(state.me) : "Operator key",
          ),
          h("span", { class: "account-role" }, state.me.role),
        ),
        h("span", { class: "nav-text account-more" }, icon(icons.more)),
      ),
    ),
  );

  shell = h(
    "div",
    { class: "shell" },
    nav,
    h("main", { class: "main" }, topbarHost, outlet),
    // Shown only with the drawer: it dims the page and catches the tap
    // outside the drawer.
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

// Rebuilt on every route, not once at boot: creating the first organisation
// makes the switcher appear, and that should not need a reload.
function renderTopbar() {
  replace(topbarHost, topbar());
}

function topbar() {
  const toggle = iconButton(icons.menu, "Menu", () => setDrawer(!drawerOpen), {
    quiet: true,
    cls: "nav-toggle",
    "aria-controls": "sidebar",
  });
  applyDrawerButton(toggle);

  const bar = h(
    "header",
    { class: "topbar" },
    toggle,
    h(
      "div",
      { class: "stack" },
      h("h1", { id: "page-title" }, ""),
      h("div", { class: "page-sub", id: "page-sub" }, ""),
    ),
    h("div", { class: "spacer" }),
  );

  // An operator looks at any organisation from one session. Someone in
  // several moves their session instead, because their role differs in each.
  if (state.me.unrestricted && state.orgs.length > 0) {
    const select = h(
      "select",
      {
        class: "select",
        style: { width: "auto" },
        "aria-label": "Organisation",
        onChange: async (e) => {
          leaveDetail();
          await setOrg(e.target.value);
          renderRoute();
        },
      },
      h("option", { value: "" }, "All organisations"),
      state.orgs.map((o) =>
        h("option", { value: o.id, selected: o.id === state.orgID }, o.name),
      ),
    );
    bar.append(select);
  } else if (state.me.orgs) {
    bar.append(
      h(
        "select",
        {
          class: "select",
          style: { width: "auto" },
          "aria-label": "Organisation",
          onChange: (e) => switchOrg(e.target),
        },
        state.me.orgs.map((o) =>
          h(
            "option",
            { value: o.org_id, selected: o.org_id === state.me.org_id },
            o.invited ? `${o.org_name} (invitation)` : o.org_name,
          ),
        ),
      ),
    );
  } else if (state.me.org_name) {
    bar.append(h("span", { class: "pill" }, state.me.org_name));
  }
  return bar;
}

function setTitle(title, sub) {
  const t = document.getElementById("page-title");
  const s = document.getElementById("page-sub");
  if (t) t.textContent = title;
  if (s) s.textContent = sub || "";
}

/** teardowns are what the current screen must undo before the next, such as
 *  closing the request log's live stream: an EventSource nobody closes keeps
 *  its connection, and a browser allows very few per origin. */
let teardowns = [];

/** rendering counts renderRoute calls. A screen that finishes loading after
 *  the reader moved on is stale: its teardowns run at once and what it drew
 *  is dropped. */
let rendering = 0;

function tearDown() {
  const pending = teardowns;
  teardowns = [];
  for (const fn of pending) {
    try {
      fn();
    } catch {
      // A screen that is already gone must not stop the next one being drawn.
    }
  }
}

/** limitedNotice tells a self-signed-up organisation which features are not
 *  open to it yet, before a refusal does. While the first run asks an
 *  administrator for credit, its credit step says it instead. Granted credit
 *  ticks that step off but does not lift the limit, so the banner returns. */
function limitedNotice() {
  if (!state.me.org_limited || state.orgID !== state.me.org_id) return null;
  const asksCredit =
    state.me.payments &&
    state.me.can_admin_org &&
    !state.setup.credits &&
    !state.setup.invoiced;
  if (asksCredit) return null;
  return banner(
    "info",
    { style: { marginBottom: "16px" } },
    "This organisation is new. " +
      // With payments on, a model on the deployment's keys is paid from
      // credit before it is used, so it is open already.
      (state.me.billing && !state.me.payments
        ? "Models on this deployment's provider keys, models and MCP " +
          "servers inside the network, such as Keera Engine, and sandboxes"
        : "Models inside the network, such as Keera Engine, MCP servers " +
          "inside the network, and sandboxes") +
      " are not open to it yet. " +
      (state.me.payments
        ? "Its first card payment opens them."
        : "An operator can lift this."),
  );
}

async function renderRoute() {
  tearDown();
  const gen = ++rendering;
  const available = visibleRoutes();
  const { route, param } = matchRoute(available, location.pathname);

  // A detail screen lights up its list: someone reading one project is still
  // under Projects.
  const current = route.under || (route.parent || route).path;
  for (const el of document.querySelectorAll(".nav-item")) {
    if (el.dataset.path === current) el.setAttribute("aria-current", "page");
    else el.removeAttribute("aria-current");
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
    reload: renderRoute,
    /** setSetup takes counts a screen has read again, so the sidebar follows
     *  steps done since the panel loaded. */
    setSetup: (setup) => {
      state.setup = setup;
      markHome();
    },
    /** setAccess does the same for a member's guide. */
    setAccess: (access) => {
      state.access = access;
      markHome();
    },
    navigate,
    setOrg,
    /** param is the path segment a detail screen was opened with, such as a
     *  project id or a model alias. Empty on every other screen. */
    param,
    setSubtitle: (s) => setTitle(title, s),
    onTeardown: (fn) => (gen === rendering ? teardowns.push(fn) : fn()),
    /** setTitle is for a screen headed by what it shows: one project is
     *  called by its own name, not "Project". */
    setTitle: (t, s) => {
      title = t;
      setTitle(t, s);
    },
  };
  // These screens belong to one organisation, so an operator looking at all
  // of them picks one first.
  const perOrg = route.parent ? route.detail.perOrg : route.perOrg;
  try {
    const view =
      perOrg && !state.orgID && state.me.unrestricted
        ? chooseOrg(ctx, perOrg)
        : await route.view(ctx);
    if (gen !== rendering) return;
    replace(outlet, limitedNotice(), view);
  } catch (err) {
    if (gen !== rendering) return;
    if (err instanceof ApiError && err.status === 401) {
      return boot(); // the session expired underneath us
    }
    replace(
      outlet,
      banner("bad", err.message || "Something went wrong loading this page."),
    );
  }
}

function accountMenu(anchor) {
  const me = state.me;
  menu(
    anchor,
    [
      me.email
        ? h(
            "div",
            { class: "menu-who" },
            h("strong", {}, personName(me)),
            me.name ? h("span", {}, me.email) : null,
          )
        : null,
      me.email ? { separator: true } : null,
      me.passkey_account
        ? {
            label: "Your passkeys",
            icon: icons.passkey,
            onClick: managePasskeys,
          }
        : null,
      {
        label: "Theme",
        icon: icons.theme,
        hint: { system: "Follows your system", light: "Light", dark: "Dark" }[
          currentTheme()
        ],
        onClick: cycleTheme,
      },
      me.chat_url
        ? { label: "Keera Chat", icon: icons.external, href: me.chat_url }
        : null,
      { label: "Documentation", icon: icons.docs, href: docsURL },
      { separator: true },
      { label: "Sign out", icon: icons.signout, onClick: signOut },
    ],
    { up: true },
  );
}

async function signOut() {
  try {
    const res = await api.signOut();
    if (res && res.provider_logout_url) {
      location.href = res.provider_logout_url;
      return;
    }
  } catch {
    // Signing out locally is what matters, even if the provider refuses.
  }
  location.href = "/";
}

async function boot() {
  applyTheme(currentTheme());
  root.replaceChildren();
  root.classList.add("boot");
  root.append(h("div", { class: "boot-mark" }));

  // A set-up link, or a passkey or password sign-in for `keera login`, comes
  // first: each is for whoever holds the link, even in a browser that is
  // signed in.
  const setup = location.hash.match(/^#passkey-setup=([A-Za-z0-9_-]+)$/);
  if (setup) return passkeySetupView(root, setup[1], boot);
  if (passkeySignInFlow() || passwordSignInFlow()) return signIn(root, boot);
  if (signUpFlow()) return signUp(root, boot);

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
          banner("bad", err.message || "The control plane is unreachable."),
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
    const last = remembered("keera.org", { lasting: true }) || "";
    if (last && state.orgs.some((o) => o.id === last)) state.orgID = last;
    else if (!state.orgID && state.orgs.length === 1)
      state.orgID = state.orgs[0].id;
  }

  // Read before the shell is drawn, because the sidebar is built from it and
  // should not grow entries a moment after it appears.
  await loadSetup();
  renderShell();
  await renderRoute();
}

/** loadSetup reads what the organisation has set up, which decides the
 *  optional screens in the sidebar. A failure leaves the counts at zero,
 *  which folds those screens away rather than breaking the panel. */
async function loadSetup() {
  try {
    state.setup = await api.setup(state.orgID);
  } catch {
    state.setup = {};
  }
  state.access = isAdmin({ state })
    ? null
    : await api.access().catch(() => null);
  markHome();
}

/** switchOrg moves the session to the selected organisation. The person's
 *  role changes with it, so the whole panel starts again. */
async function switchOrg(select) {
  select.disabled = true;
  try {
    await api.switchOrg(select.value);
  } catch (err) {
    select.value = state.me.org_id;
    select.disabled = false;
    toast(err.message || "Could not switch the organisation.", "bad");
    return;
  }
  leaveDetail();
  await boot();
}

/** leaveDetail moves a detail screen back to its list before the organisation
 *  changes, because what it shows belongs to the old one. */
function leaveDetail() {
  const { route } = matchRoute(visibleRoutes(), location.pathname);
  if (route.parent) history.replaceState({}, "", route.parent.path);
}

/** setOrg changes the organisation every screen is about and redraws the
 *  sidebar. The caller then draws the screen. */
async function setOrg(id) {
  state.orgID = id;
  remember("keera.org", id || null, { lasting: true });
  await loadSetup();
  renderShell();
}

boot();
