// The cards for a router's steps and a guardrail's blocks, so the two editors
// look and behave alike.

import { h, icon, icons } from "../ui.js";

/** nodeCard is one step or block. glyph is an icon's path, or an element
 *  such as a provider's mark. With onOpen the card opens on a click or Enter.
 *  It is a div acting as a button, so the buttons in its head can be real. */
export function nodeCard({
  kind,
  glyph,
  label,
  name,
  meta,
  problem,
  title,
  onOpen,
  buttons,
}) {
  return h(
    "div",
    {
      class:
        "pl-node pl-kind-" +
        kind +
        (problem ? " pl-node-bad" : "") +
        (onOpen ? " pl-node-live" : ""),
      role: onOpen ? "button" : null,
      tabindex: onOpen ? "0" : null,
      title: onOpen ? title : null,
      onClick: onOpen || null,
      onKeydown: onOpen
        ? (e) => {
            if (e.target !== e.currentTarget) return;
            if (e.key !== "Enter" && e.key !== " ") return;
            e.preventDefault();
            onOpen();
          }
        : null,
    },
    h(
      "div",
      { class: "pl-node-head" },
      typeof glyph === "string" ? icon(glyph) : glyph,
      h("span", { class: "pl-node-kind" }, label),
      h("div", { class: "spacer" }),
      buttons,
    ),
    h("div", { class: "pl-node-name" }, name),
    meta,
    problem ? h("div", { class: "pl-node-problem" }, problem) : null,
  );
}

/** headButton does not open the card. */
export function headButton(glyph, title, onClick, cls) {
  return h(
    "button",
    {
      class: "pl-node-btn" + (cls ? " " + cls : ""),
      type: "button",
      title,
      "aria-label": title,
      onClick: (e) => {
        e.stopPropagation();
        onClick();
      },
    },
    glyph,
  );
}

export function removeButton(title, onClick) {
  return headButton(icon(icons.close), title, onClick, "pl-node-remove");
}

/** emptyCard is where a step or block can be added. Without onClick it only
 *  shows the place is empty. */
export function emptyCard({ label, name, note, problem, title, onClick }) {
  return h(
    onClick ? "button" : "div",
    {
      class: "pl-node pl-node-empty",
      type: onClick ? "button" : null,
      title: onClick ? title : null,
      onClick: onClick || null,
    },
    h(
      "div",
      { class: "pl-node-head" },
      icon(icons.plus),
      h("span", { class: "pl-node-kind" }, label),
    ),
    h("div", { class: "pl-node-name" }, name),
    note ? h("div", { class: "faint small" }, note) : null,
    problem ? h("div", { class: "pl-node-problem" }, problem) : null,
  );
}

export function addButton(title, onClick) {
  return h(
    "button",
    { class: "pl-add", type: "button", title, "aria-label": title, onClick },
    icon(icons.plus),
  );
}
