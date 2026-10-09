// The sign-in screen. Single sign-on when an identity provider is configured,
// and the operator key when it is not - which is how a deployment is set up
// before its identity provider exists. Where both work, single sign-on is the
// screen and the operator key is a link under it: it is for setting the
// deployment up and for one-off operator tasks, not for the people signing in
// every day.

import { api, ApiError } from "../api.js";
import { getPasskey, passkeysSupported } from "../webauthn.js";
import { h, replace, icon, icons, brandMark, idpMark } from "../ui.js";

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
    // Fall back to offering the operator key: it is the path that works when
    // nothing else is configured, which includes "we cannot ask".
  }

  const params = new URLSearchParams(location.search);
  const ssoError = params.get("sign_in_error");
  // Where the person was going, kept through any way of signing in.
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
    if (!value) return;
    button.disabled = true;
    button.textContent = "Signing in…";
    try {
      await api.localSignIn(value);
      history.replaceState({}, "", "/");
      await onSignedIn();
    } catch (err) {
      replace(
        message,
        h(
          "div",
          { class: "banner banner-bad" },
          err instanceof ApiError ? err.message : "Sign-in failed.",
        ),
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
      style: { width: "100%", justifyContent: "center" },
    },
    "Sign in with the operator key",
  );

  const keyForm = h(
    "form",
    { onSubmit: submit },
    h(
      "div",
      { class: "field" },
      h("label", { for: "operator-key" }, "Operator key"),
      keyInput,
      h(
        "div",
        { class: "hint" },
        "The value of KEERA_OPERATOR_KEY. It signs you in as an operator.",
      ),
    ),
    button,
  );

  const card = h(
    "div",
    { class: "signin-card" },
    h("div", { class: "signin-brand" }, brandMark(28), "Keera Gateway"),
    ssoError
      ? h("div", { class: "banner banner-bad" }, "Sign-in failed: " + ssoError)
      : null,
    message,
  );

  // focusKey is set when the screen ends up showing the key field, so that the
  // cursor lands in it once the card is actually in the document.
  let focusKey = false;

  const cliFlow = passkeySignInFlow();
  const passkeyButton = (primary) => {
    const b = h(
      "button",
      {
        class: "btn" + (primary ? " btn-primary" : ""),
        style: { width: "100%", justifyContent: "center" },
        onClick: async () => {
          replace(message);
          b.disabled = true;
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
            replace(
              message,
              h(
                "div",
                { class: "banner banner-bad" },
                err.message || "Signing in with a passkey failed.",
              ),
            );
            b.disabled = false;
          }
        },
      },
      icon(icons.passkey),
      "Sign in with a passkey",
    );
    if (!passkeysSupported()) b.disabled = true;
    return b;
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
    // The operator key is how a deployment is set up before its identity
    // provider exists, and how an operator does the odd task afterwards. Once
    // single sign-on works it is the wrong door for everyone who is not doing
    // one of those two things, so it is off this screen entirely: an operator
    // who needs it comes to ?operator_key=1, and nobody else is offered it.
    const operatorKey = params.get("operator_key") === "1";
    focusKey = operatorKey;

    // Each directory is named: somebody with both a work Google account and a
    // work Microsoft one cannot answer "single sign-on", and picking wrong
    // lands them in a failed sign-in rather than a different button.
    const providers = config.providers || [];
    const signUpWith = providers.find((p) => p.signup);

    card.append(
      h(
        "div",
        { class: "signin-providers" },
        ...providers.map((p, i) =>
          h(
            "a",
            {
              // The first is the primary button and the rest are plain. It is
              // not a ranking of directories - there is no way to know which
              // one a given reader belongs to - but a stack of identical
              // primary buttons reads as a choice that matters more than this
              // one does.
              class: "btn" + (i === 0 ? " btn-primary" : ""),
              style: { width: "100%", justifyContent: "center" },
              href: api.loginURL({ provider: p.name, next }),
            },
            idpMark(p.name) || icon(icons.signout),
            "Continue with " + (p.label || p.name),
          ),
        ),
        config.passkeys ? passkeyButton(false) : null,
      ),
      h(
        "div",
        { class: "signin-note" },
        // Deliberately not "ask to be put in the right group": on a deployment
        // whose roles are assigned in Keera there is no group to be put in, and
        // on Google Workspace there could not be one.
        (config.passkeys
          ? "Sign in with your identity provider or a passkey. "
          : "Sign in with your identity provider. ") +
          (signUpWith
            ? "New here? Continue with " +
              (signUpWith.label || signUpWith.name) +
              " to create an organisation."
            : "If you cannot, ask your administrator."),
      ),
      ...(operatorKey
        ? [h("div", { class: "divider" }, "operator"), keyForm]
        : []),
    );
  } else if (config.passkeys) {
    // No directory, so the operator key stays on the screen: a passkey
    // account is never an operator, so this is the only way to be one.
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
