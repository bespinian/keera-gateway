// The sign-in screen. Single sign-on when an identity provider is configured,
// and the operator key when it is not - which is how a deployment is set up
// before its identity provider exists. Where both work, single sign-on is the
// screen and the operator key is a link under it: it is for setting the
// deployment up and for one-off operator tasks, not for the people signing in
// every day.

import { api, ApiError } from "../api.js";
import { h, replace, icon, icons, brandMark, idpMark } from "../ui.js";

export async function signIn(root, onSignedIn) {
  let config = { sso: false, providers: [], operator_key: true };
  try {
    config = await api.authConfig();
  } catch {
    // Fall back to offering the operator key: it is the path that works when
    // nothing else is configured, which includes "we cannot ask".
  }

  const params = new URLSearchParams(location.search);
  const ssoError = params.get("sign_in_error");

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

  const keyForm =
    config.operator_key === false
      ? null
      : h(
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
      ? h(
          "div",
          { class: "banner banner-bad" },
          "Single sign-on failed: " + ssoError,
        )
      : null,
    message,
  );

  // focusKey is set when the screen ends up showing the key field, so that the
  // cursor lands in it once the card is actually in the document.
  let focusKey = false;

  if (config.sso) {
    // The operator key is how a deployment is set up before its identity
    // provider exists, and how an operator does the odd task afterwards. Once
    // single sign-on works it is the wrong door for everyone who is not doing
    // one of those two things, so it is off this screen entirely: an operator
    // who needs it comes to ?operator_key=1, and nobody else is offered it.
    const operatorKey = keyForm !== null && params.get("operator_key") === "1";
    focusKey = operatorKey;

    // A deployment with one directory has nothing to choose between, so its
    // button does not ask - it says "single sign-on" and the person behind it
    // finds out which directory when they get there. Where there are several,
    // each is named: somebody with both a work Google account and a work
    // Microsoft one cannot answer "single sign-on", and picking wrong lands
    // them in a failed sign-in rather than a different button.
    const providers =
      config.providers && config.providers.length
        ? config.providers
        : [{ name: "", label: "single sign-on" }];
    const next = params.get("next");

    card.append(
      h(
        "div",
        { class: "signin-providers" },
        ...providers.map((p, i) => {
          const query = new URLSearchParams();
          if (p.name) query.set("provider", p.name);
          if (next) query.set("next", next);
          return h(
            "a",
            {
              // The first is the primary button and the rest are plain. It is
              // not a ranking of directories - there is no way to know which
              // one a given reader belongs to - but a stack of identical
              // primary buttons reads as a choice that matters more than this
              // one does.
              class: "btn" + (i === 0 ? " btn-primary" : ""),
              style: { width: "100%", justifyContent: "center" },
              href:
                "/control/auth/login" +
                (query.size ? "?" + query.toString() : ""),
            },
            idpMark(p.name) || icon(icons.signout),
            "Continue with " + (p.label || p.name),
          );
        }),
      ),
      h(
        "div",
        { class: "signin-note" },
        // Deliberately not "ask to be put in the right group": on a deployment
        // whose roles are assigned in Keera there is no group to be put in, and
        // on Google Workspace there could not be one.
        "Sign in with your identity provider. If you cannot, ask your " +
          "administrator.",
      ),
      ...(operatorKey
        ? [h("div", { class: "divider" }, "operator"), keyForm]
        : []),
    );
  } else if (keyForm) {
    card.append(
      keyForm,
      h(
        "div",
        { class: "signin-note" },
        "No identity provider is set up, so only the operator key works. For " +
          "single sign-on, set KEERA_OIDC_ISSUER, KEERA_OIDC_CLIENT_ID, " +
          "KEERA_OIDC_CLIENT_SECRET and KEERA_OIDC_REDIRECT_URL, or " +
          "KEERA_OIDC_PROVIDERS for more than one provider.",
      ),
    );
    focusKey = true;
  } else {
    // Neither way in is configured. Saying so beats a screen with nothing on
    // it, which reads as a panel that failed to load.
    card.append(
      h(
        "div",
        { class: "banner banner-bad" },
        "There is no way to sign in: no identity provider and no operator " +
          "key. Set KEERA_OPERATOR_KEY, or KEERA_OIDC_ISSUER and its client " +
          "settings, then restart.",
      ),
    );
  }

  root.classList.remove("boot");
  replace(root, h("div", { class: "signin" }, card));
  if (focusKey) keyInput.focus();
}
