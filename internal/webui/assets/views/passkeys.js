// Passkeys: setting one up from a link, your own list, and an administrator's
// view of someone else's.
//
// Only accounts that sign in with passkeys have any. Directory accounts sign
// in through their identity provider, so that leaving the directory still
// locks them out.

import { api } from "../api.js";
import { createPasskey, deviceName } from "../webauthn.js";
import {
  h,
  replace,
  modal,
  toast,
  confirm,
  ago,
  icon,
  icons,
  showError,
  brandMark,
  copyText,
  dateTime,
} from "../ui.js";

/** passkeySetupView is the page a set-up link opens. It adds a passkey to the
 *  account the link is for, and signs the person in. */
export async function passkeySetupView(root, token, onDone) {
  root.classList.remove("boot");
  const message = h("div");
  const card = h(
    "div",
    { class: "signin-card" },
    h("div", { class: "signin-brand" }, brandMark(28), "Keera Gateway"),
    message,
  );
  replace(root, h("div", { class: "signin" }, card));

  let opts;
  try {
    opts = await api.passkeySetupOptions(token);
  } catch (err) {
    showError(message, err.message);
    card.append(
      h(
        "a",
        {
          class: "btn",
          href: "/",
          style: { width: "100%", justifyContent: "center" },
        },
        "Go to sign-in",
      ),
    );
    return;
  }

  const name = h("input", {
    class: "input",
    id: "passkey-name",
    value: deviceName(),
    maxlength: 64,
  });
  const button = h(
    "button",
    {
      class: "btn btn-primary",
      type: "submit",
      style: { width: "100%", justifyContent: "center" },
    },
    icon(icons.passkey),
    "Create passkey",
  );
  const submit = async (e) => {
    e.preventDefault();
    showError(message, "");
    button.disabled = true;
    try {
      // The challenge works once, so a retry asks for a new one.
      const fresh = opts || (await api.passkeySetupOptions(token));
      opts = null;
      const credential = await createPasskey(fresh.options);
      await api.passkeySetup(
        token,
        fresh.challenge_id,
        name.value.trim(),
        credential,
      );
      history.replaceState({}, "", "/");
      await onDone();
    } catch (err) {
      showError(message, err.message);
      button.disabled = false;
    }
  };

  card.append(
    h("p", {}, "Set up a passkey for ", h("strong", {}, opts.email), "."),
    h(
      "p",
      { class: "muted" },
      "A passkey lets you sign in with your fingerprint, face or device PIN. " +
        "There is no password.",
    ),
    h(
      "form",
      { onSubmit: submit },
      h(
        "div",
        { class: "field" },
        h("label", { for: "passkey-name" }, "Name"),
        name,
        h(
          "div",
          { class: "hint" },
          "So you can tell your passkeys apart later.",
        ),
      ),
      button,
    ),
  );
}

/** managePasskeys is your own list: add one for another device, remove one
 *  you no longer use. */
export function managePasskeys() {
  const err = h("div");
  const list = h("div");

  const draw = async () => {
    let keys;
    try {
      keys = (await api.passkeys()).data || [];
    } catch (ex) {
      showError(err, ex.message);
      return;
    }
    replace(
      list,
      passkeyRows(keys, (k) =>
        keys.length > 1
          ? h(
              "button",
              {
                class: "btn btn-sm btn-danger",
                title: "Remove this passkey",
                "aria-label": `Remove ${k.name}`,
                onClick: () =>
                  confirm({
                    title: `Remove “${k.name}”?`,
                    body: "You can no longer sign in with it.",
                    confirmLabel: "Remove",
                    danger: true,
                    onConfirm: async () => {
                      await api.deletePasskey(k.id);
                      toast("Passkey removed", "good");
                      draw();
                    },
                  }),
              },
              icon(icons.trash),
            )
          : h(
              "span",
              {
                class: "faint",
                title: "Add another before you remove this one",
              },
              "your only one",
            ),
      ),
    );
  };

  const add = async (e) => {
    const button = e.currentTarget;
    button.disabled = true;
    showError(err, "");
    try {
      const opts = await api.ownPasskeyOptions();
      const credential = await createPasskey(opts.options);
      await api.addPasskey(opts.challenge_id, deviceName(), credential);
      toast("Passkey added", "good");
      draw();
    } catch (ex) {
      showError(err, ex.message);
    } finally {
      button.disabled = false;
    }
  };

  modal({
    title: "Your passkeys",
    subtitle: "Add one on each device you sign in from.",
    body: h("div", {}, err, list),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Close"),
      h(
        "button",
        { class: "btn btn-primary", onClick: add },
        icon(icons.plus),
        "Add a passkey on this device",
      ),
    ],
  });
  draw();
}

/** userPasskeys is an administrator's view of one person's passkeys. */
export function userPasskeys(user) {
  const err = h("div");
  const list = h("div");

  const draw = async () => {
    let keys;
    try {
      keys = (await api.passkeys(user.id)).data || [];
    } catch (ex) {
      showError(err, ex.message);
      return;
    }
    replace(
      list,
      keys.length
        ? passkeyRows(keys, (k) =>
            h(
              "button",
              {
                class: "btn btn-sm btn-danger",
                title: "Remove this passkey",
                "aria-label": `Remove ${k.name}`,
                onClick: () =>
                  confirm({
                    title: `Remove “${k.name}”?`,
                    body:
                      `${user.email} can no longer sign in with it, and is ` +
                      "signed out everywhere.",
                    confirmLabel: "Remove",
                    danger: true,
                    onConfirm: async () => {
                      await api.deletePasskey(k.id);
                      toast("Passkey removed", "good");
                      draw();
                    },
                  }),
              },
              icon(icons.trash),
            ),
          )
        : h(
            "p",
            { class: "muted" },
            "No passkey yet. Send them a set-up link to add their first.",
          ),
    );
  };

  modal({
    title: `Passkeys of ${user.email}`,
    subtitle:
      "A set-up link adds a passkey, for a new device or after a lost one.",
    body: h("div", {}, err, list),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Close"),
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: async () => {
            try {
              const link = await api.passkeyLink(user.id);
              close();
              showPasskeyLink(user.email, link);
            } catch (ex) {
              showError(err, ex.message);
            }
          },
        },
        "New set-up link",
      ),
    ],
  });
  draw();
}

/** showPasskeyLink hands over a set-up link. It is shown once. */
export function showPasskeyLink(email, link) {
  const value = h("input", {
    class: "input key-value mono",
    readonly: true,
    value: link.url,
    "aria-label": "The set-up link",
    onFocus: (e) => e.target.select(),
    onClick: (e) => e.target.select(),
  });
  modal({
    title: `Set-up link for ${email}`,
    body: h(
      "div",
      {},
      h(
        "div",
        { class: "field" },
        h(
          "div",
          { class: "row-tight" },
          value,
          h(
            "button",
            { class: "btn", onClick: () => copyText(link.url) },
            icon(icons.copy),
            "Copy",
          ),
        ),
        h(
          "div",
          { class: "hint" },
          `Send it to ${email} privately: whoever opens it can add a passkey ` +
            `to this account. It works once, until ${dateTime(link.expires_at)}. ` +
            "A new link replaces this one.",
        ),
      ),
    ),
    actions: (close) => [
      h("button", { class: "btn btn-primary", onClick: close }, "Done"),
    ],
  });
}

function passkeyRows(keys, action) {
  return h(
    "div",
    { class: "stack" },
    ...keys.map((k) =>
      h(
        "div",
        { class: "row", style: { gap: "12px", padding: "8px 0" } },
        h("span", { class: "passkey-mark" }, icon(icons.passkey)),
        h(
          "div",
          { class: "stack", style: { flex: 1 } },
          h("strong", {}, k.name),
          h(
            "span",
            { class: "faint", style: { fontSize: "11px" } },
            `added ${ago(k.created_at)}` +
              (k.last_used_at
                ? `, last used ${ago(k.last_used_at)}`
                : ", never used"),
          ),
        ),
        action(k),
      ),
    ),
  );
}
