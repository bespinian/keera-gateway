// The sign-in screen: a button per identity provider, a password form for
// LDAP, and passkeys where they are on. With none of these, or at
// ?operator_key=1, it asks for the operator key, which sets up a new
// deployment. It also hands a sign-in over to `keera login` or Keera Chat.

import { api, ApiError } from "../api.js";
import { getPasskey, passkeysSupported } from "../webauthn.js";
import {
  h,
  replace,
  icon,
  icons,
  brandMark,
  idpMark,
  showError,
  field,
  banner,
} from "../ui.js";

/** passkeySignInFlow is the passkey sign-in /auth/login started, as
 *  { flow, to }, or null. It comes after the #, so it never reaches a server
 *  log. */
export function passkeySignInFlow() {
  const m = location.hash.match(
    /^#passkey-sign-in=([A-Za-z0-9_-]+)(?:\.(cli|chat))?$/,
  );
  return m ? { flow: m[1], to: m[2] || "" } : null;
}

/** passwordSignInFlow is the LDAP sign-in /auth/login started, as
 *  { provider, flow, to }, or null. */
export function passwordSignInFlow() {
  const m = location.hash.match(
    /^#password-sign-in=([a-z0-9][a-z0-9-]*)\.([A-Za-z0-9_-]+)(?:\.(cli|chat))?$/,
  );
  return m ? { provider: m[1], flow: m[2], to: m[3] || "" } : null;
}

/** handOver is the text around a form that finishes a sign-in started
 *  elsewhere. to is where it goes next. */
function handOver(to) {
  if (to === "cli") {
    return [
      "Sign in to the keera command line.",
      "Your browser then hands the sign-in over to the terminal.",
    ];
  }
  if (to === "chat") {
    return [
      "Sign in to Keera Chat.",
      "Your browser then goes back to Keera Chat.",
    ];
  }
  return ["Sign in to Keera Gateway.", ""];
}

// takeSignInError is why the last sign-in failed, read once. It is in a
// cookie, not the address, which anyone can write into a link.
function takeSignInError() {
  const name = "keera_sign_in_error=";
  const found = document.cookie.split("; ").find((c) => c.startsWith(name));
  document.cookie = name + "; Max-Age=0; Path=/";
  if (!found) return "the sign-in did not finish.";
  try {
    return decodeURIComponent(found.slice(name.length));
  } catch {
    return "the sign-in did not finish.";
  }
}

export async function signIn(root, onSignedIn) {
  let config = { sso: false, providers: [], passkeys: false };
  try {
    config = await api.authConfig();
  } catch {
    // Fall back to the operator key.
  }

  const params = new URLSearchParams(location.search);
  const ssoError = params.has("sign_in_error") ? takeSignInError() : null;
  const next = params.get("next");

  const message = h("div");
  const keyInput = h("input", {
    class: "input",
    type: "password",
    id: "operator-key",
    placeholder: "keera operator key",
    autocomplete: "current-password",
  });

  const submit = async (e) => {
    e.preventDefault();
    replace(message);
    const value = keyInput.value.trim();
    if (!value) {
      showError(message, "Enter the operator key.");
      keyInput.focus();
      return;
    }
    button.disabled = true;
    button.textContent = "Signing in…";
    try {
      await api.localSignIn(value);
      history.replaceState({}, "", "/");
      await onSignedIn();
    } catch (err) {
      showError(
        message,
        err instanceof ApiError ? err.message : "Sign-in failed.",
      );
      button.disabled = false;
      button.textContent = "Sign in with the operator key";
      keyInput.focus();
      keyInput.select();
    }
  };

  const button = h(
    "button",
    {
      class: "btn btn-primary",
      type: "submit",
    },
    "Sign in with the operator key",
  );

  const keyForm = h(
    "form",
    { onSubmit: submit },
    field(
      "Operator key",
      keyInput,
      "The value of KEERA_OPERATOR_KEY. It signs you in as an operator.",
    ),
    button,
  );

  const card = h(
    "div",
    { class: "signin-card" },
    h("div", { class: "signin-brand" }, brandMark(28), "Keera Gateway"),
    ssoError ? banner("bad", "Sign-in failed: " + ssoError) : null,
    message,
  );

  // So the cursor lands in the key field once the card is shown.
  let focusKey = false;

  const passkeyFlow = passkeySignInFlow();
  const cliFlow = passkeyFlow ? passkeyFlow.flow : "";
  const cliPassword = passwordSignInFlow();

  // A sign-in started elsewhere goes back there.
  const finish = async (res, flow) => {
    if (flow) {
      location.href = res.redirect;
      return;
    }
    history.replaceState({}, "", res.redirect || "/");
    await onSignedIn();
  };

  // back, if given, returns to the other ways to sign in.
  const passwordForm = (p, flow, back) => {
    const label = "Sign in with " + (p.label || p.name);
    const user = h("input", {
      class: "input",
      id: "password-user",
      autocomplete: "username",
      autocapitalize: "none",
      spellcheck: "false",
    });
    const pass = h("input", {
      class: "input",
      type: "password",
      id: "password-pass",
      autocomplete: "current-password",
    });
    const b = h(
      "button",
      {
        class: "btn btn-primary",
        type: "submit",
      },
      label,
    );
    const form = h(
      "form",
      {
        onSubmit: async (e) => {
          e.preventDefault();
          replace(message);
          if (!user.value.trim() || !pass.value) {
            showError(message, "Enter your username and password.");
            (user.value.trim() ? pass : user).focus();
            return;
          }
          b.disabled = true;
          b.textContent = "Signing in…";
          try {
            const res = await api.passwordSignIn(
              p.name,
              flow,
              user.value,
              pass.value,
              next,
            );
            await finish(res, flow);
          } catch (err) {
            showError(message, err.message || "Sign-in failed.");
            // Never kept, not even in the field.
            pass.value = "";
            b.disabled = false;
            b.textContent = label;
            pass.focus();
          }
        },
      },
      field("Username", user, "Your " + (p.label || p.name) + " account."),
      field("Password", pass),
      b,
      back
        ? h(
            "button",
            {
              class: "btn btn-quiet",
              type: "button",
              style: { marginTop: "8px" },
              onClick: back,
            },
            "Other ways to sign in",
          )
        : null,
    );
    queueMicrotask(() => user.focus());
    return form;
  };
  const passkeyButton = (primary) => {
    const supported = passkeysSupported();
    const label = (text) => [icon(icons.passkey), text];
    const b = h(
      "button",
      {
        class: "btn" + (primary ? " btn-primary" : ""),
        disabled: !supported,
        title: supported ? null : "This browser does not support passkeys",
        onClick: async () => {
          replace(message);
          b.disabled = true;
          replace(b, label("Signing in…"));
          try {
            const opts = await api.passkeySignInOptions(cliFlow, next);
            const credential = await getPasskey(opts.options);
            const res = await api.passkeySignIn(opts.flow, credential);
            await finish(res, cliFlow);
          } catch (err) {
            showError(
              message,
              err.message || "Signing in with a passkey failed.",
            );
            b.disabled = false;
            replace(b, label("Sign in with a passkey"));
          }
        },
      },
      label("Sign in with a passkey"),
    );
    // A disabled button shows no tooltip in some browsers, so the reason is
    // also written under it.
    if (supported) return b;
    return [
      b,
      h(
        "div",
        { class: "hint", style: { textAlign: "center" } },
        "This browser does not support passkeys.",
      ),
    ];
  };

  if (cliPassword) {
    // /auth/login sent the browser here, such as for `keera login`.
    const p = (config.providers || []).find(
      (p) => p.name === cliPassword.provider,
    ) || { name: cliPassword.provider };
    const [title, note] = handOver(cliPassword.to);
    card.append(
      h("p", {}, title),
      passwordForm(p, cliPassword.flow),
      ...(note ? [h("div", { class: "signin-note" }, note)] : []),
    );
  } else if (cliFlow) {
    // /auth/login sent the browser here, such as for `keera login`. Only the
    // passkey finishes it.
    const [title, note] = handOver(passkeyFlow.to);
    card.append(
      h("p", {}, title),
      h("div", { class: "signin-providers" }, passkeyButton(true)),
      ...(note ? [h("div", { class: "signin-note" }, note)] : []),
    );
  } else if (config.sso) {
    // Once single sign-on works, the operator key is offered only at
    // ?operator_key=1.
    const operatorKey = params.get("operator_key") === "1";
    focusKey = operatorKey;

    // Each directory is named: someone with Google and Microsoft accounts
    // cannot answer "single sign-on".
    const providers = config.providers || [];
    const signUpWith = providers.find((p) => p.signup);

    // A password directory opens its form in place of the buttons. If it is
    // the only choice, it is open from the start.
    const box = h("div", { class: "signin-providers" });
    const showButtons = () => {
      replace(message);
      replace(
        box,
        ...providers.map((p) =>
          p.password
            ? h(
                "button",
                {
                  class: "btn",
                  type: "button",
                  onClick: () => {
                    replace(message);
                    replace(box, passwordForm(p, "", showButtons));
                  },
                },
                icon(icons.signout),
                "Sign in with " + (p.label || p.name),
              )
            : h(
                "a",
                {
                  class: "btn",
                  href: api.loginURL({ provider: p.name, next }),
                },
                idpMark(p.name) || icon(icons.signout),
                "Sign in with " + (p.label || p.name),
              ),
        ),
        config.passkeys ? passkeyButton(false) : null,
      );
    };
    if (providers.length === 1 && providers[0].password && !config.passkeys) {
      replace(box, passwordForm(providers[0], ""));
    } else {
      showButtons();
    }

    card.append(
      box,
      h(
        "div",
        { class: "signin-note" },
        // Not "ask to be put in the right group": where roles are set in
        // Keera, or on Google Workspace, there is no such group.
        (config.passkeys
          ? "Sign in with your identity provider or a passkey. "
          : "Sign in with your identity provider. ") +
          (signUpWith
            ? "New here? Sign in with " +
              (signUpWith.label || signUpWith.name) +
              " to create an organisation."
            : "If you cannot, ask your administrator."),
      ),
      ...(operatorKey
        ? [h("div", { class: "divider" }, "operator"), keyForm]
        : []),
    );
  } else if (config.passkeys) {
    // No directory, so the operator key stays: a passkey account is never an
    // operator.
    card.append(
      h("div", { class: "signin-providers" }, passkeyButton(true)),
      h(
        "div",
        { class: "signin-note" },
        "Sign in with the passkey you set up. If you have none, ask your " +
          "administrator for a set-up link.",
      ),
      h("div", { class: "divider" }, "operator"),
      keyForm,
    );
  } else {
    card.append(
      keyForm,
      h(
        "div",
        { class: "signin-note" },
        "No identity provider is set up, so only the operator key works. For " +
          "single sign-on, set KEERA_PUBLIC_URL, name the provider in " +
          "KEERA_OIDC_PROVIDERS and set its KEERA_OIDC_<NAME>_ISSUER, _CLIENT_ID " +
          "and _CLIENT_SECRET.",
      ),
    );
    focusKey = true;
  }

  root.classList.remove("boot");
  replace(root, h("div", { class: "signin" }, card));
  if (focusKey) keyInput.focus();
}
