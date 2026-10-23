// The sign-in screen: single sign-on when an identity provider is configured,
// and the operator key when not, which is how a new deployment is set up.

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

/** passkeySignInFlow is the sign-in `keera login --provider passkey` started,
 *  if this page was opened for one. It comes after the #, so it never reaches
 *  a server log. */
export function passkeySignInFlow() {
  const m = location.hash.match(/^#passkey-sign-in=([A-Za-z0-9_-]+)$/);
  return m ? m[1] : "";
}

export async function signIn(root, onSignedIn) {
  let config = { sso: false, providers: [], passkeys: false };
  try {
    config = await api.authConfig();
  } catch {
    // Fall back to the operator key, which works when nothing else does.
  }

  const params = new URLSearchParams(location.search);
  const ssoError = params.has("sign_in_error") ? takeSignInError() : null;
  // Where the person was going.
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

  // Set when the key field is shown, so the cursor lands in it once the card
  // is in the document.
  let focusKey = false;

  const cliFlow = passkeySignInFlow();
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
            if (cliFlow) {
              // On to the port `keera login` is waiting on.
              location.href = res.redirect;
              return;
            }
            history.replaceState({}, "", res.redirect || "/");
            await onSignedIn();
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

  if (cliFlow) {
    // `keera login` sent the browser here. Only the passkey finishes it.
    card.append(
      h("p", {}, "Sign in to the keera command line."),
      h("div", { class: "signin-providers" }, passkeyButton(true)),
      h(
        "div",
        { class: "signin-note" },
        "Your browser then hands the sign-in over to the terminal.",
      ),
    );
  } else if (config.sso) {
    // Once single sign-on works, the operator key is only for setup and the odd
    // operator task, so it is offered only at ?operator_key=1.
    const operatorKey = params.get("operator_key") === "1";
    focusKey = operatorKey;

    // Each directory is named: someone with both a Google and a Microsoft
    // account cannot answer "single sign-on".
    const providers = config.providers || [];
    const signUpWith = providers.find((p) => p.signup);

    card.append(
      h(
        "div",
        { class: "signin-providers" },
        ...providers.map((p) =>
          h(
            "a",
            {
              class: "btn",
              style: { width: "100%", justifyContent: "center" },
              href: api.loginURL({ provider: p.name, next }),
            },
            idpMark(p.name) || icon(icons.signout),
            "Sign in with " + (p.label || p.name),
          ),
        ),
        config.passkeys ? passkeyButton(false) : null,
      ),
      h(
        "div",
        { class: "signin-note" },
        // Not "ask to be put in the right group": where roles are assigned in
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
