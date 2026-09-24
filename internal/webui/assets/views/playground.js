// The playground: a conversation with one model.
//
// The first question anyone asks of a new gateway is whether the models behind
// it actually answer, and the honest way to find out is to ask one. The request
// goes through the same data plane an editor's does - same allow-list, same
// rate limits, same budget, same usage row - so an answer here means the path a
// developer will use works, not that a second path does.

import { api, ApiError } from "../api.js";
import {
  h,
  icon,
  icons,
  replace,
  copyText,
  money,
  num,
  ms,
  empty,
} from "../ui.js";

/** session outlives a route change, so stepping over to Usage to look at what a
 *  message cost and stepping back does not throw the conversation away. It does
 *  not outlive a reload: a scratchpad that comes back from the dead days later
 *  is a surprise, not a feature. */
const session = {
  model: "",
  system: "",
  temperature: "",
  maxTokens: "",
  messages: [],
};

export async function playgroundView(ctx) {
  const catalogue = ((await api.models()).data || []).filter(
    (m) => m.enabled && m.kind === "chat",
  );
  // The organisation's routers, offered where its chat models are. It is worth
  // having here for a reason the models are not: a router's whole behaviour is
  // a judgement about a prompt, and this is the one screen in the panel where
  // somebody can type a prompt and see which model it reached. The check does
  // that with three fixed samples; this does it with the question actually
  // being argued about.
  if (ctx.orgID) {
    const routers = await api
      .routers(ctx.orgID)
      .then((r) => r.data || [])
      .catch(() => []);
    for (const rt of routers) {
      catalogue.push({
        alias: rt.alias,
        kind: "chat",
        enabled: true,
        router: true,
        description: rt.description,
      });
    }
  }
  const models = await usable(ctx, catalogue);

  if (!ctx.orgID) {
    // Only an operator can be looking at every organisation at once. There is
    // no sensible default for whose budget a message comes out of.
    return h(
      "div",
      { class: "banner banner-info" },
      "Pick an organisation in the header first. Messages sent here are " +
        "rate-limited and charged to it, like any other request.",
    );
  }
  if (!models.length) {
    return h(
      "div",
      { class: "card" },
      empty(
        catalogue.length
          ? "No chat model is available to you"
          : "The catalogue has no chat model",
        catalogue.length
          ? "This organisation's guardrails allow no model that serves chat."
          : "Add a chat model under Models.",
      ),
    );
  }
  if (!models.some((m) => m.alias === session.model))
    session.model = models[0].alias;

  return chat(ctx, models);
}

/** usable narrows the catalogue to what this organisation is actually allowed
 *  to reach, so the picker cannot offer a model that only ever answers 404. */
async function usable(ctx, catalogue) {
  if (!ctx.orgID) return catalogue;
  try {
    const limits = await api.guardrails("org", ctx.orgID);
    const allowed = limits && limits.allowed_models;
    if (!allowed || !allowed.length) return catalogue;
    return catalogue.filter((m) => allowed.includes(m.alias));
  } catch {
    // Not being able to read the guardrails is not a reason to show nothing;
    // the gateway refuses a model this organisation may not use anyway.
    return catalogue;
  }
}

/* ------------------------------------------------------------------- chat */

function chat(ctx, models) {
  const byAlias = new Map(models.map((m) => [m.alias, m]));
  let abort = null;

  const thread = h("div", { class: "thread" });
  const scroll = h("div", { class: "chat-scroll" }, thread);
  const modelSelect = h(
    "select",
    {
      class: "select",
      style: { width: "auto" },
      "aria-label": "Model",
      onChange: () => {
        session.model = modelSelect.value;
        ctx.setSubtitle(subtitle(byAlias.get(session.model)));
        // The empty state names the model, so it has to follow the picker. A
        // full redraw is only safe while the thread is empty: mid-stream it
        // would detach the node paint() is writing tokens into.
        if (!session.messages.length) render();
      },
    },
    models.map((m) =>
      h(
        "option",
        { value: m.alias, selected: m.alias === session.model },
        m.alias,
      ),
    ),
  );

  const settings = settingsPanel();
  const input = h("textarea", {
    class: "input composer-input",
    rows: "1",
    placeholder: "Send a message…",
    "aria-label": "Message",
    onInput: () => autosize(input),
    onKeyDown: (e) => {
      // Enter sends and shift-Enter breaks the line, which is what every chat
      // client does and therefore what fingers already expect.
      if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        submit();
      }
    },
  });

  const sendBtn = h(
    "button",
    {
      class: "btn btn-primary composer-send",
      type: "submit",
      title: "Send",
      "aria-label": "Send",
    },
    icon(icons.send),
  );
  const stopBtn = h(
    "button",
    {
      class: "btn composer-send",
      type: "button",
      hidden: true,
      title: "Stop generating",
      "aria-label": "Stop generating",
      onClick: () => abort && abort.abort(),
    },
    icon(icons.stop),
  );

  const clearBtn = h(
    "button",
    {
      class: "btn btn-sm",
      title: "Clear the conversation",
      onClick: () => {
        if (abort) abort.abort();
        session.messages = [];
        render();
        input.focus();
      },
    },
    icon(icons.trash),
    "Clear",
  );

  const bar = h(
    "div",
    { class: "chat-bar" },
    modelSelect,
    h("div", { style: { flex: 1 } }),
    h(
      "button",
      {
        class: "btn btn-sm",
        "aria-expanded": String(!settings.el.hidden),
        onClick: (e) => {
          settings.el.hidden = !settings.el.hidden;
          e.currentTarget.setAttribute(
            "aria-expanded",
            String(!settings.el.hidden),
          );
        },
      },
      icon(icons.sliders),
      "Parameters",
    ),
    clearBtn,
  );

  const form = h(
    "form",
    {
      class: "composer",
      onSubmit: (e) => {
        e.preventDefault();
        submit();
      },
    },
    input,
    sendBtn,
    stopBtn,
  );

  const root = h(
    "div",
    { class: "chat" },
    bar,
    settings.el,
    scroll,
    h(
      "div",
      { class: "composer-wrap" },
      form,
      h(
        "div",
        { class: "composer-hint" },
        "Enter sends · Shift+Enter breaks the line · charged to ",
        h("strong", {}, ctx.state.me.org_name || "this organisation"),
      ),
    ),
  );

  ctx.setSubtitle(subtitle(byAlias.get(session.model)));

  /* ---------------------------------------------------------- rendering */

  // pinned tracks whether the reader is at the bottom. Dragging a stream's
  // scrollbar upward to reread something and having it yanked back down on the
  // next token is the single most irritating thing a chat view can do.
  let pinned = true;
  scroll.addEventListener("scroll", () => {
    pinned = scroll.scrollHeight - scroll.scrollTop - scroll.clientHeight < 40;
  });

  function stick() {
    if (pinned) scroll.scrollTop = scroll.scrollHeight;
  }

  function render() {
    clearBtn.disabled = session.messages.length === 0;
    if (!session.messages.length) {
      replace(
        thread,
        h(
          "div",
          { class: "chat-empty" },
          h("div", { class: "chat-empty-mark" }, icon(icons.playground)),
          h(
            "strong",
            {},
            "Ask ",
            h("span", { class: "mono" }, session.model),
            " something",
          ),
          h(
            "div",
            { class: "muted" },
            "These are real requests. Guardrails apply, and they show up " +
              "in Usage.",
          ),
        ),
      );
      return;
    }
    replace(
      thread,
      session.messages.map((m) => messageEl(m, ctx, byAlias)),
    );
    stick();
  }

  /* ----------------------------------------------------------- sending */

  async function submit() {
    const text = input.value.trim();
    if (!text || abort) return;

    // The history goes out before the placeholder joins it, so the request does
    // not carry an empty assistant turn.
    const history = wireMessages(session.messages);
    if (session.system.trim())
      history.unshift({ role: "system", content: session.system.trim() });
    history.push({ role: "user", content: text });

    input.value = "";
    autosize(input);
    const ask = { role: "user", content: text };
    session.messages.push(ask);

    const reply = {
      role: "assistant",
      model: session.model,
      content: "",
      reasoning: "",
      streaming: true,
      started: performance.now(),
    };
    session.messages.push(reply);
    render();

    abort = new AbortController();
    sendBtn.hidden = true;
    stopBtn.hidden = false;
    input.disabled = true;

    // While tokens arrive the message is redrawn as plain text on an animation
    // frame rather than per chunk: a fast model emits a chunk per token, and
    // rebuilding the DOM that often is how a stream turns into a stutter. Only
    // the two moments that change the message's shape - the first token, and
    // the first sign of reasoning - cost a rebuild.
    let node = thread.lastElementChild;
    let shape = { waiting: true, reasoning: false };
    let queued = false;
    const paint = () => {
      queued = false;
      const wantsReasoning = !!reply.reasoning;
      if (
        (shape.waiting && (reply.content || reply.reasoning)) ||
        (wantsReasoning && !shape.reasoning)
      ) {
        const fresh = messageEl(reply, ctx, byAlias);
        node.replaceWith(fresh);
        node = fresh;
        shape = { waiting: false, reasoning: wantsReasoning };
      } else {
        const live = node.querySelector(".msg-live");
        if (live) live.textContent = reply.content;
        const think = node.querySelector(".msg-reasoning-body");
        if (think) think.textContent = reply.reasoning;
      }
      stick();
    };

    try {
      const res = await api.playground(
        ctx.orgID,
        request(reply.model, history),
        abort.signal,
      );
      for await (const chunk of events(res.body)) {
        if (chunk.usage) reply.usage = chunk.usage;
        const choice = (chunk.choices || [])[0];
        const delta = choice && choice.delta;
        if (delta) {
          if (!reply.ttft && (delta.content || delta.reasoning_content)) {
            reply.ttft = performance.now() - reply.started;
          }
          if (delta.content) reply.content += delta.content;
          if (delta.reasoning_content)
            reply.reasoning += delta.reasoning_content;
        }
        if (choice && choice.finish_reason) reply.finish = choice.finish_reason;
        if (!queued) {
          queued = true;
          requestAnimationFrame(paint);
        }
      }
    } catch (err) {
      if (err && err.name === "AbortError") reply.stopped = true;
      else if (err instanceof ApiError) reply.error = err.message;
      else
        reply.error =
          (err && err.message) || "The response ended unexpectedly.";
      // A refused turn never reached the model, so it drops out of the history
      // rather than leaving a question with no answer behind it - two user
      // turns in a row is something a chat template is entitled to reject.
      if (reply.error) ask.unsent = true;
    } finally {
      reply.streaming = false;
      reply.latency = performance.now() - reply.started;
      abort = null;
      sendBtn.hidden = false;
      stopBtn.hidden = true;
      input.disabled = false;
      // The finished message is drawn once more in full, which is where fenced
      // code becomes a code block - mid-stream a fence is usually still open.
      render();
      input.focus();
    }
  }

  function request(model, messages) {
    const payload = {
      model,
      messages,
      stream: true,
      // Asking for the usage record explicitly is what makes the gateway pass
      // it through rather than swallow the chunk it added for its own billing.
      stream_options: { include_usage: true },
    };
    const temp = parseFloat(session.temperature);
    if (Number.isFinite(temp)) payload.temperature = temp;
    const max = parseInt(session.maxTokens, 10);
    if (Number.isFinite(max) && max > 0) payload.max_tokens = max;
    return payload;
  }

  render();
  queueMicrotask(() => input.focus());
  return root;
}

function subtitle(model) {
  if (!model) return "";
  // A router has no backend model and no context window of its own: which
  // model answers is not decided until the message has been read, so what is
  // worth saying is that it will be decided at all.
  if (model.router) {
    return "a router: it picks the model";
  }
  const bits = [model.backend_model];
  if (model.max_context) bits.push(num(model.max_context) + " token context");
  return bits.filter(Boolean).join(" · ");
}

/** wireMessages is the conversation as the inference plane should see it:
 *  turns that carry text, and nothing that failed. */
function wireMessages(messages) {
  return messages
    .filter((m) => !m.error && !m.unsent && m.content)
    .map((m) => ({ role: m.role, content: m.content }));
}

/* ------------------------------------------------------------- transport */

/** events yields one decoded chunk per server-sent event. */
async function* events(stream) {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true }).replace(/\r\n/g, "\n");
    let i;
    while ((i = buf.indexOf("\n\n")) >= 0) {
      const event = buf.slice(0, i);
      buf = buf.slice(i + 2);
      for (const line of event.split("\n")) {
        if (!line.startsWith("data:")) continue;
        const payload = line.slice(5).trim();
        if (payload === "" || payload === "[DONE]") continue;
        try {
          yield JSON.parse(payload);
        } catch {
          // A chunk that is not JSON is not something to show a person; the
          // stream carries on and the message stands on what did parse.
        }
      }
    }
  }
}

/* ------------------------------------------------------------ components */

function settingsPanel() {
  const system = h("textarea", {
    class: "input",
    rows: "3",
    placeholder: "You are a helpful assistant.",
    onInput: (e) => {
      session.system = e.target.value;
    },
  });
  system.value = session.system;

  const temperature = h("input", {
    class: "input",
    type: "number",
    min: "0",
    max: "2",
    step: "0.1",
    placeholder: "model default",
    value: session.temperature,
    onInput: (e) => {
      session.temperature = e.target.value;
    },
  });
  const maxTokens = h("input", {
    class: "input",
    type: "number",
    min: "1",
    step: "1",
    placeholder: "model default",
    value: session.maxTokens,
    onInput: (e) => {
      session.maxTokens = e.target.value;
    },
  });

  const el = h(
    "div",
    { class: "card card-body chat-settings", hidden: true },
    h(
      "div",
      { class: "field" },
      h("label", {}, "System prompt"),
      system,
      h(
        "div",
        { class: "hint" },
        "Sent before the conversation with every message.",
      ),
    ),
    h(
      "div",
      { class: "field-row" },
      h("div", { class: "field" }, h("label", {}, "Temperature"), temperature),
      h(
        "div",
        { class: "field" },
        h("label", {}, "Max output tokens"),
        maxTokens,
        h("div", { class: "hint" }, "A lower guardrail limit still applies."),
      ),
    ),
  );
  return { el };
}

function messageEl(msg, ctx, byAlias) {
  if (msg.role === "user") {
    return h(
      "div",
      { class: "msg msg-user" },
      h(
        "div",
        { class: "bubble" + (msg.unsent ? " bubble-unsent" : "") },
        ...prose(msg.content),
      ),
      msg.unsent
        ? h(
            "div",
            { class: "msg-foot" },
            "Refused, so not part of the conversation.",
          )
        : null,
    );
  }

  const body = h("div", { class: "msg-body" });
  if (msg.streaming) body.append(h("div", { class: "msg-live" }, msg.content));
  else if (msg.content) body.append(...prose(msg.content));

  if (msg.streaming && !msg.content && !msg.reasoning) {
    body.append(
      h(
        "div",
        { class: "msg-waiting" },
        h("span", { class: "blip" }),
        "Waiting for the first token…",
      ),
    );
  }

  return h(
    "div",
    { class: "msg msg-assistant" },
    h(
      "div",
      { class: "msg-head" },
      h("span", { class: "mono msg-model" }, msg.model),
      h("div", { style: { flex: 1 } }),
      !msg.streaming && msg.content
        ? h(
            "button",
            {
              class: "btn btn-quiet btn-sm",
              title: "Copy",
              "aria-label": "Copy the reply",
              onClick: () => copyText(msg.content),
            },
            icon(icons.copy),
          )
        : null,
    ),
    msg.reasoning ? reasoningEl(msg) : null,
    body,
    msg.error ? h("div", { class: "banner banner-bad" }, msg.error) : null,
    msg.stopped ? h("div", { class: "faint msg-foot" }, "Stopped.") : null,
    !msg.streaming && !msg.error ? footer(msg, ctx, byAlias) : null,
  );
}

/** reasoningEl shows a model's thinking, folded away. It is worth having -
 *  a reasoning model that produces nothing else looks broken without it. */
function reasoningEl(msg) {
  const el = h(
    "details",
    { class: "msg-reasoning", open: msg.streaming && !msg.content },
    h("summary", {}, "Reasoning"),
    h("div", { class: "msg-reasoning-body" }, msg.reasoning),
  );
  return el;
}

function footer(msg, ctx, byAlias) {
  const bits = [];
  if (msg.usage) {
    // Read off the response body the browser got back from /api/v1/chat/completions
    // rather than out of a Keera report, so these are the OpenAI field names and
    // not the input/output the rest of the dashboard reads. The playground is
    // the one screen whose numbers come straight off the wire.
    bits.push(
      `${num(msg.usage.prompt_tokens)} in`,
      `${num(msg.usage.completion_tokens)} out`,
    );
    const model = byAlias.get(msg.model);
    if (
      model &&
      (model.input_micros_per_mtok || model.output_micros_per_mtok)
    ) {
      const cost =
        Math.round(
          ((msg.usage.prompt_tokens || 0) * model.input_micros_per_mtok) / 1e6,
        ) +
        Math.round(
          ((msg.usage.completion_tokens || 0) * model.output_micros_per_mtok) /
            1e6,
        );
      bits.push(money(cost, ctx.currency));
    }
  }
  if (msg.ttft) bits.push(ms(msg.ttft) + " to first token");
  if (msg.latency) bits.push(ms(msg.latency) + " total");
  if (msg.finish && msg.finish !== "stop") bits.push("finish: " + msg.finish);
  if (!bits.length) return null;
  return h("div", { class: "msg-foot" }, bits.join(" · "));
}

/** prose renders a reply. Fenced blocks become code and everything else stays
 *  as typed - every node is built from text, so nothing a model generates is
 *  ever handed to the browser as markup. */
function prose(text) {
  const out = [];
  const parts = String(text).split(/\n?```/);
  parts.forEach((part, i) => {
    if (i % 2 === 0) {
      const text2 = part.replace(/^\n+|\n+$/g, "");
      if (text2) out.push(h("p", { class: "msg-text" }, text2));
      return;
    }
    // The first line of a fence is the language, when there is one.
    const nl = part.indexOf("\n");
    const lang = nl < 0 ? "" : part.slice(0, nl).trim();
    const code = nl < 0 ? part : part.slice(nl + 1);
    out.push(
      h(
        "div",
        { class: "codeblock" },
        lang ? h("div", { class: "codeblock-lang" }, lang) : null,
        h("pre", {}, h("code", {}, code.replace(/\n$/, ""))),
      ),
    );
  });
  return out;
}

function autosize(el) {
  el.style.height = "auto";
  el.style.height = Math.min(el.scrollHeight, 260) + "px";
}
