(() => {
  let source,
    activeChat,
    activeRoot,
    queue = Promise.resolve();
  const pending = new Map();
  const drafts = new Map();
  const nearBottom = () => {
    const el = document.querySelector("#conversation");
    return !el || el.scrollHeight - el.scrollTop - el.clientHeight < 100;
  };
  const scroll = () => {
    const el = document.querySelector("#conversation");
    if (el) el.scrollTop = el.scrollHeight;
  };
  function flush() {
    const follow = nearBottom();
    for (const [id, text] of pending) {
      const el = document.querySelector(`#item-${id} .message-body`);
      if (el) el.textContent = text;
    }
    pending.clear();
    if (follow) scroll();
  }
  setInterval(flush, 75);
  async function swap(target, text, style = "innerHTML") {
    if (target)
      await htmx.swap({ target, text, swap: style, sourceElement: target });
  }
  function ready() {
    const input = document.querySelector("#message");
    if (input)
      input.disabled =
        document.querySelector("#turn-controls")?.dataset.ready !== "true";
  }
  function connect() {
    const root = document.querySelector("#workspace");
    if (!root || root === activeRoot) {
      ready();
      return;
    }
    source?.close();
    pending.clear();
    activeRoot = root;
    activeChat = root.dataset.chat;
    const input = document.querySelector("#message");
    if (input) input.value = drafts.get(activeChat) || "";
    const stream = new EventSource(
      `/events?chat=${encodeURIComponent(activeChat)}`,
    );
    source = stream;
    stream.onopen = () => {
      if (source !== stream) return;
      const el = document.querySelector("#connection");
      if (el) {
        el.textContent = "Connected";
        el.classList.add("online");
      }
    };
    stream.onerror = () => {
      if (source !== stream) return;
      const el = document.querySelector("#connection");
      if (el) {
        el.textContent = "Reconnecting…";
        el.classList.remove("online");
      }
    };
    stream.onmessage = (event) => {
      queue = queue
        .then(async () => {
          if (source !== stream) return;
          const data = JSON.parse(event.data),
            follow = nearBottom();
          if (data.snapshot) {
            pending.clear();
            await swap(
              document.querySelector("#conversation"),
              data.conversation || "",
            );
          }
          for (const item of data.items || []) {
            if (source !== stream) return;
            document.querySelector("#empty-conversation")?.remove();
            const el = document.getElementById(`item-${item.id}`);
            if (item.text !== undefined && el) {
              pending.set(item.id, item.text);
            } else {
              pending.delete(item.id);
              const opened = el?.querySelector("details")?.open;
              await swap(
                el || document.querySelector("#conversation"),
                item.html,
                el ? "outerHTML" : "beforeend",
              );
              if (opened)
                document
                  .querySelector(`#item-${item.id} details`)
                  ?.setAttribute("open", "");
            }
          }
          if (source !== stream) return;
          await swap(root, data.html, "none");
          ready();
          if (follow || data.snapshot) scroll();
        })
        .catch((error) =>
          showError(`Could not update the conversation: ${error.message}`),
        );
    };
    ready();
    scroll();
  }
  function showError(message) {
    const el = document.querySelector("#app-error");
    if (el) {
      el.textContent = message;
      el.hidden = false;
    }
  }
  document.addEventListener("htmx:after:swap", connect);
  document.addEventListener("htmx:after:request", (event) => {
    const ctx = event.detail.ctx;
    if (ctx.response.status >= 400) {
      showError(ctx.text);
      return;
    }
    const error = document.querySelector("#app-error");
    if (error) error.hidden = true;
    if (ctx.sourceElement.id === "message-form") {
      ctx.sourceElement.reset();
      drafts.delete(activeChat);
    }
  });
  document.addEventListener("htmx:error", () =>
    showError("Could not reach Hypercode. Check the connection and try again."),
  );
  document.addEventListener("input", (event) => {
    if (event.target.id === "message")
      drafts.set(activeChat, event.target.value);
    if (event.target.matches("[data-free-answer]")) {
      const fieldset = event.target.closest("fieldset");
      fieldset.querySelectorAll("input[type=radio]").forEach((radio) => {
        if (event.target.value) radio.checked = false;
      });
    }
  });
  // The new-chat form renders one model select per agent. Only the chosen
  // agent's select is visible and enabled, so only it is submitted. (Hiding
  // <option> elements is not honored by WebKit, hence whole selects.)
  const filterModels = () => {
    const harness = document.querySelector("#harness");
    if (!harness) return;
    for (const select of document.querySelectorAll(".model-select")) {
      const active = select.dataset.harness === harness.value;
      select.hidden = !active;
      select.disabled = !active;
    }
  };
  document.addEventListener("change", (event) => {
    if (event.target.id === "harness") filterModels();
    if (event.target.matches("input[type=radio]")) {
      const text = event.target
        .closest("fieldset")
        ?.querySelector("[data-free-answer]");
      if (text) text.value = "";
    }
  });
  // Omit empty free-text inputs when a radio answer was selected.
  document.addEventListener("htmx:before:request", (event) => {
    const body = event.detail.ctx.request.body;
    if (body instanceof FormData || body instanceof URLSearchParams) {
      for (const key of new Set(body.keys())) {
        if (key.startsWith("question-")) {
          const values = body
            .getAll(key)
            .filter((value) => typeof value === "string" && value.trim());
          body.delete(key);
          for (const value of values) body.append(key, value);
        }
      }
    }
  });
  document.addEventListener("keydown", (event) => {
    if (
      event.target.id === "message" &&
      event.key === "Enter" &&
      !event.shiftKey &&
      !event.isComposing
    ) {
      event.preventDefault();
      if (!event.target.disabled) event.target.form.requestSubmit();
    }
    if (
      event.key.toLowerCase() === "n" &&
      !event.metaKey &&
      !event.ctrlKey &&
      !event.altKey &&
      !event.target.matches("input,textarea,select,[contenteditable]")
    ) {
      event.preventDefault();
      document.querySelector(".new-chat")?.click();
    }
  });
  window.addEventListener("pagehide", () => {
    source?.close();
    source = null;
    activeRoot = null;
    pending.clear();
  });
  window.addEventListener("pageshow", (event) => {
    if (event.persisted) connect();
  });
  document.addEventListener("htmx:after:swap", filterModels);
  document.addEventListener("DOMContentLoaded", () => {
    connect();
    filterModels();
  });
})();
