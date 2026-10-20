// The playground: a conversation with one model, to check the models answer.
// Messages go out with one of the reader's own keys, through the same data
// plane an editor uses, so they meet that key's guardrails, budgets and usage.

import { api, ApiError, chatTargets } from "../api.js";
import {
  h,
  icon,
  icons,
  iconButton,
  replace,
  copyText,
  money,
  num,
  compact,
  ms,
  toast,
  field,
  banner,
} from "../ui.js";
import { keyModels, noKey, noModel, usableKeys } from "./connect.js";

/** session outlives a route change, so a look at Usage does not lose the
 *  conversation. It does not outlive a reload on purpose: a chat that comes
 *  back days later is a surprise. */
const session = {
  keyID: "",
  model: "",
  system: "",
  temperature: "",
  maxTokens: "",
  messages: [],
};

export async function playgroundView(ctx) {
  // Only the reader's own keys, because a message spends the key's budget.
  const a = await api.access();
  const keys = usableKeys(a);
  if (!keys.length) return noKey(ctx, a);
  if (!keys.some((k) => k.id === session.keyID)) session.keyID = keys[0].id;
  const key = keys.find((k) => k.id === session.keyID);

  // Routers are offered too: this is the one screen that shows which model a
  // router picks. Only what the key may call is offered, so nothing answers 404.
  const models = keyModels(await chatTargets(key.org_id), key.allowed_models);
  if (!models.length) {
    return h(
      "div",
      { class: "card" },
      h("div", { class: "card-body" }, keyPicker(ctx, keys)),
      noModel(),
    );
  }
  if (!models.some((m) => m.alias === session.model))
    session.model = models[0].alias;

  return chat(ctx, keys, key, models);
}

/** Changing the key redraws the screen, because the models on offer depend
 *  on it. */
function keyPicker(ctx, keys, onChange) {
  const el = h(
    "select",
    {
      class: "select",
      style: { width: "auto" },
      "aria-label": "API key",
      onChange: () => {
        if (onChange) onChange();
        session.keyID = el.value;
        ctx.reload();
      },
    },
    keys.map((k) =>
      h(
        "option",
        { value: k.id, selected: k.id === session.keyID },
        k.name || k.prefix,
      ),
    ),
  );
  return el;
}

/* ------------------------------------------------------------------- chat */

function chat(ctx, keys, key, models) {
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
      // Enter sends and shift-Enter breaks the line, as in every chat client.
      if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        submit();
      }
    },
  });

  // Images wait here until the message they belong to is sent.
  let attached = [];
  const tray = h("div", { class: "composer-tray", hidden: true });
  const picker = h("input", {
    type: "file",
    accept: imageTypes,
    multiple: true,
    hidden: true,
    onChange: async () => {
      const files = [...picker.files];
      // Cleared, so choosing the same file again still fires a change.
      picker.value = "";
      for (const file of files) {
        try {
          attached.push({ name: file.name, url: await readImage(file) });
        } catch {
          toast(`${file.name} is not an image this browser can read.`, "bad");
        }
      }
      paintTray();
      input.focus();
    },
  });
  const attachBtn = h(
    "button",
    {
      class: "btn composer-send",
      type: "button",
      title: "Attach an image",
      "aria-label": "Attach an image",
      onClick: () => picker.click(),
    },
    icon(icons.plus),
  );

  function paintTray() {
    tray.hidden = attached.length === 0;
    replace(
      tray,
      attached.map((a) =>
        h(
          "div",
          { class: "composer-thumb" },
          h("img", { src: a.url, alt: a.name }),
          h(
            "button",
            {
              class: "composer-thumb-remove",
              type: "button",
              title: "Remove",
              "aria-label": `Remove ${a.name}`,
              onClick: () => {
                attached = attached.filter((x) => x !== a);
                paintTray();
                // The button is gone, so focus would be lost with it.
                input.focus();
              },
            },
            icon(icons.close),
          ),
        ),
      ),
    );
  }

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
    keyPicker(ctx, keys, () => abort && abort.abort()),
    modelSelect,
    h("div", { class: "spacer" }),
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
    attachBtn,
    input,
    sendBtn,
    stopBtn,
    picker,
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
      tray,
      form,
      h(
        "div",
        { class: "composer-hint" },
        "Enter sends · Shift+Enter breaks the line · sent with ",
        h("strong", {}, key.name || key.prefix),
      ),
    ),
  );

  ctx.setSubtitle(subtitle(byAlias.get(session.model)));

  /* ---------------------------------------------------------- rendering */

  // pinned tracks whether the reader is at the bottom, so a reader who
  // scrolled up to reread is not pulled back down on the next token.
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
            "These are real requests, sent with your key. Its guardrails " +
              "and budgets apply, and they show up in Usage.",
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
    const images = attached.map((a) => a.url);
    if ((!text && !images.length) || abort) return;

    // The history goes out before the placeholder joins it, so the request does
    // not carry an empty assistant turn.
    const history = wireMessages(session.messages);
    if (session.system.trim())
      history.unshift({ role: "system", content: session.system.trim() });
    history.push({ role: "user", content: wireContent(text, images) });

    input.value = "";
    autosize(input);
    attached = [];
    paintTray();
    const ask = { role: "user", content: text, images };
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
    attachBtn.disabled = true;

    // While tokens arrive the message is redrawn as plain text once per
    // animation frame, not per chunk, or a fast model makes the stream stutter.
    // Only the first token and the first reasoning cost a full rebuild.
    let node = thread.lastElementChild;
    let shape = { waiting: true, reasoning: false };
    let queued = false;
    let raw = "";
    let thought = "";
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
        session.keyID,
        request(reply.model, history),
        abort.signal,
      );
      for await (const chunk of events(res.body)) {
        if (chunk.usage) reply.usage = chunk.usage;
        const choice = (chunk.choices || [])[0];
        const delta = choice && choice.delta;
        if (delta) {
          // Newer vLLM servers name the field "reasoning", older ones and
          // llama.cpp "reasoning_content".
          const more = delta.reasoning || delta.reasoning_content;
          if (!reply.ttft && (delta.content || more)) {
            reply.ttft = performance.now() - reply.started;
          }
          if (delta.content) raw += delta.content;
          if (more) thought += more;
          const split = splitThink(raw);
          reply.content = split.content;
          reply.reasoning = thought + split.reasoning;
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
      // A refused turn never reached the model, so it drops out of the history:
      // a chat template may reject two user turns in a row.
      if (reply.error) ask.unsent = true;
    } finally {
      reply.streaming = false;
      reply.latency = performance.now() - reply.started;
      abort = null;
      sendBtn.hidden = false;
      stopBtn.hidden = true;
      input.disabled = false;
      attachBtn.disabled = false;
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
      // Without asking, the gateway swallows the usage chunk it added for its
      // own billing.
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
  // A router has no model or context window of its own: it picks one only
  // once it has read the message.
  if (model.router) {
    return "A router: it picks the model";
  }
  const bits = [model.display_name || model.backend_model];
  if (model.max_context_tokens)
    bits.push(compact(model.max_context_tokens) + " token context");
  return bits.filter(Boolean).join(" · ");
}

/** splitThink takes the thinking out of a reply that writes it inline as
 *  <think>…</think>, as MiniMax does, rather than in a reasoning field. Some
 *  chat templates open the tag in the prompt, so the reply only closes it. */
function splitThink(text) {
  const open = text.match(/^\s*<think>/);
  const end = text.indexOf("</think>");
  if (!open && end < 0) return { content: text, reasoning: "" };
  const from = open ? open[0].length : 0;
  if (end < 0) return { content: "", reasoning: text.slice(from).trim() };
  return {
    content: text.slice(end + "</think>".length).trimStart(),
    reasoning: text.slice(from, end).trim(),
  };
}

/** wireMessages keeps the turns that carry text or images, and drops any
 *  that failed. */
function wireMessages(messages) {
  return messages
    .filter((m) => !m.error && !m.unsent && (m.content || m.images?.length))
    .map((m) => ({ role: m.role, content: wireContent(m.content, m.images) }));
}

/** wireContent uses the OpenAI parts form only for turns with images, which
 *  only models that read images accept. */
function wireContent(text, images) {
  if (!images || !images.length) return text;
  const parts = images.map((url) => ({
    type: "image_url",
    image_url: { url },
  }));
  if (text) parts.unshift({ type: "text", text });
  return parts;
}

/* ----------------------------------------------------------------- images */

// The formats every API that reads images accepts.
const imageTypes = "image/png,image/jpeg,image/gif,image/webp";

// Providers scale larger images down to about this size anyway, and every
// image is sent again with each later message.
const maxImageSide = 2048;

async function readImage(file) {
  const url = await new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result);
    reader.onerror = () => reject(reader.error);
    reader.readAsDataURL(file);
  });
  // Also checks that the file is an image at all.
  const bitmap = await createImageBitmap(file);
  const scale = maxImageSide / Math.max(bitmap.width, bitmap.height);
  // A GIF is kept as it is, because redrawing it would drop its animation.
  if (scale >= 1 || file.type === "image/gif") {
    bitmap.close();
    return url;
  }
  const canvas = h("canvas", {
    width: Math.round(bitmap.width * scale),
    height: Math.round(bitmap.height * scale),
  });
  canvas.getContext("2d").drawImage(bitmap, 0, 0, canvas.width, canvas.height);
  bitmap.close();
  // A browser that cannot write the file's own format writes PNG.
  return canvas.toDataURL(file.type, 0.9);
}

/* ------------------------------------------------------------- transport */

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
          // A chunk that is not JSON is skipped; the stream carries on.
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
    field(
      "System prompt",
      system,
      "Sent before the conversation with every message.",
    ),
    h(
      "div",
      { class: "field-row" },
      field("Temperature", temperature),
      field(
        "Max output tokens",
        maxTokens,
        "A lower guardrail limit still applies.",
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
        msg.images?.length
          ? h(
              "div",
              { class: "bubble-images" },
              msg.images.map((url) => h("img", { src: url, alt: "" })),
            )
          : null,
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
    { class: "msg" },
    h(
      "div",
      { class: "msg-head" },
      h("span", { class: "mono msg-model" }, msg.model),
      h("div", { class: "spacer" }),
      !msg.streaming && msg.content
        ? iconButton(icons.copy, "Copy", () => copyText(msg.content), {
            quiet: true,
            label: "Copy the reply",
          })
        : null,
    ),
    msg.reasoning ? reasoningEl(msg) : null,
    body,
    msg.error ? banner("bad", msg.error) : null,
    msg.stopped ? h("div", { class: "faint msg-foot" }, "Stopped.") : null,
    !msg.streaming && !msg.error ? footer(msg, ctx, byAlias) : null,
  );
}

/** reasoningEl shows a model's thinking, folded away. Without it, a model
 *  that only reasons looks broken. */
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
    // Read straight off the chat completions response, not a Keera report, so
    // these are OpenAI field names rather than input/output.
    bits.push(
      `${num(msg.usage.prompt_tokens)} in`,
      `${num(msg.usage.completion_tokens)} out`,
    );
    const model = byAlias.get(msg.model);
    if (
      model &&
      (model.input_micros_per_mtok || model.output_micros_per_mtok)
    ) {
      // The same sum as policy.Model.Cost: cache reads and writes are part of
      // the prompt, each at its own rate, or the input rate when none is set.
      // A prompt over the long-prompt threshold is charged those prices.
      const input = msg.usage.prompt_tokens || 0;
      const details = msg.usage.prompt_tokens_details || {};
      const cached = Math.min(Math.max(details.cached_tokens || 0, 0), input);
      const written = Math.min(
        Math.max(details.cache_write_tokens || 0, 0),
        input - cached,
      );
      const long = model.long_prompt;
      const rates = long && input > long.above_tokens ? long : model;
      const cachedRate =
        rates.cached_input_micros_per_mtok || rates.input_micros_per_mtok;
      const writeRate =
        rates.cache_write_micros_per_mtok || rates.input_micros_per_mtok;
      const cost = Math.floor(
        ((input - cached - written) * rates.input_micros_per_mtok +
          cached * cachedRate +
          written * writeRate +
          (msg.usage.completion_tokens || 0) * rates.output_micros_per_mtok) /
          1e6,
      );
      bits.push(money(cost));
    }
  }
  if (msg.ttft) bits.push(ms(msg.ttft) + " to first token");
  if (msg.latency) bits.push(ms(msg.latency) + " total");
  if (msg.finish && msg.finish !== "stop") bits.push("finish: " + msg.finish);
  if (!bits.length) return null;
  return h("div", { class: "msg-foot" }, bits.join(" · "));
}

/** prose renders a reply: fenced blocks become code, the rest stays as typed.
 *  Every node is built from text, so model output is never parsed as markup. */
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
