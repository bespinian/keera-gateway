// The sign-up screen. A sign-in that matched no organisation lands here, and
// the person creates one, or joins the one that added their address. Nothing
// is joined without asking: anyone who runs an organisation can add any
// address to it.

import { api, ApiError } from "../api.js";
import { h, replace, brandMark } from "../ui.js";

/** signUpFlow reports whether this page was opened for a sign-up. */
export function signUpFlow() {
  return new URLSearchParams(location.search).get("sign_up") === "1";
}

export async function signUp(root, onSignedIn) {
  root.classList.remove("boot");
  const card = h(
    "div",
    { class: "signin-card" },
    h("div", { class: "signin-brand" }, brandMark(28), "Keera Gateway"),
  );
  replace(root, h("div", { class: "signin" }, card));

  let info;
  try {
    info = await api.signupInfo();
  } catch (err) {
    card.append(
      h(
        "div",
        { class: "banner banner-bad" },
        err instanceof ApiError ? err.message : "This sign-up cannot be read.",
      ),
      h("a", { class: "btn", href: "/" }, "Sign in again"),
    );
    return;
  }

  const message = h("div");
  const fail = (err) =>
    replace(
      message,
      h(
        "div",
        { class: "banner banner-bad" },
        err instanceof ApiError ? err.message : "Something went wrong.",
      ),
    );
  const finish = async (body, button) => {
    replace(message);
    button.disabled = true;
    try {
      const res = await api.completeSignup(body);
      history.replaceState({}, "", res.redirect || "/");
      await onSignedIn();
    } catch (err) {
      fail(err);
      button.disabled = false;
    }
  };

  const invited = info.invited_org;
  const nameInput = h("input", {
    class: "input",
    id: "org-name",
    maxlength: 100,
    placeholder: "Your company or team",
    autocomplete: "organization",
  });
  const create = h(
    "button",
    {
      class: "btn" + (invited ? "" : " btn-primary"),
      type: "submit",
      style: { width: "100%", justifyContent: "center" },
    },
    "Create organisation",
  );
  const createForm = h(
    "form",
    {
      onSubmit: (e) => {
        e.preventDefault();
        const name = nameInput.value.trim();
        if (!name) {
          nameInput.focus();
          return;
        }
        finish({ name }, create);
      },
    },
    h(
      "div",
      { class: "field" },
      h("label", { for: "org-name" }, "Name of your organisation"),
      nameInput,
      h("div", { class: "hint" }, "You become its administrator."),
    ),
    create,
  );

  card.append(
    h("p", {}, "You signed in as ", h("strong", {}, info.email), "."),
    message,
  );
  if (invited) {
    const join = h(
      "button",
      {
        class: "btn btn-primary",
        style: { width: "100%", justifyContent: "center" },
        onClick: () => finish({ join: true }, join),
      },
      "Join " + invited,
    );
    card.append(
      h("p", {}, invited + " has added your address."),
      h("div", { class: "signin-providers" }, join),
      h("div", { class: "divider" }, "or create your own"),
      createForm,
    );
  } else {
    card.append(
      h("p", {}, "There is no organisation for this address yet."),
      createForm,
      h(
        "div",
        { class: "signin-note" },
        "If your company already uses Keera, ask its administrator to add " +
          "you instead.",
      ),
    );
    nameInput.focus();
  }
}
