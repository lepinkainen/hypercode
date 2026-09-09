(() => {
  let source,
    activeChat,
    activeRoot,
    queue = Promise.resolve();
  const pending = new Map();
  const drafts = new Map();
  const sending = new Map();
  const submissions = new WeakMap();
  function draft(id = activeChat) {
    if (!drafts.has(id)) drafts.set(id, { text: "", files: [] });
    return drafts.get(id);
  }
  function releaseDraft(value) {
    for (const entry of value.files)
      if (entry.url) URL.revokeObjectURL(entry.url);
  }
  const fileSize = (n) =>
    n < 1024
      ? `${n} B`
      : n < 1048576
        ? `${(n / 1024).toFixed(1)} KB`
        : `${(n / 1048576).toFixed(1)} MB`;
  function renderAttachments() {
    const list = document.querySelector("#draft-attachments");
    if (!list) return;
    list.replaceChildren();
    for (const [index, entry] of draft().files.entries()) {
      const card = document.createElement("div");
      card.className = "attachment";
      if (entry.url) {
        const img = document.createElement("img");
        img.src = entry.url;
        img.alt = entry.file.name;
        card.append(img);
      }
      const name = document.createElement("span");
      name.textContent = entry.file.name;
      const size = document.createElement("small");
      size.textContent = fileSize(entry.file.size);
      name.append(size);
      const remove = document.createElement("button");
      remove.type = "button";
      remove.dataset.removeAttachment = index;
      remove.setAttribute("aria-label", `Remove ${entry.file.name}`);
      remove.textContent = "×";
      card.append(name, remove);
      list.append(card);
    }
    ready();
  }
  function addFiles(files) {
    const form = document.querySelector("#message-form");
    if (!form || document.querySelector("#message")?.disabled) return;
    const value = draft();
    const incoming = Array.from(files);
    const all = [...value.files.map((entry) => entry.file), ...incoming];
    if (all.length > Number(form.dataset.maxFiles)) {
      showError(`Attach up to ${form.dataset.maxFiles} files.`);
      return;
    }
    if (all.reduce((n, f) => n + f.size, 0) > Number(form.dataset.totalBytes)) {
      showError("Attachments exceed the total size limit.");
      return;
    }
    for (const file of incoming) {
      const ext = file.name.split(".").pop().toLowerCase();
      const isImage = ["png", "jpg", "jpeg", "webp"].includes(ext);
      if (
        !isImage &&
        !["txt", "md", "markdown"].includes(ext) &&
        !(ext === "pdf" && form.dataset.pdf === "true")
      ) {
        showError(`Unsupported attachment: ${file.name}`);
        return;
      }
      const limit = Number(
        isImage ? form.dataset.imageBytes : form.dataset.documentBytes,
      );
      if (!file.size || file.size > limit) {
        showError(
          `${file.name} must be between 1 byte and ${fileSize(limit)}.`,
        );
        return;
      }
    }
    for (const file of incoming)
      value.files.push({
        file,
        url: /^image\/(png|jpeg|webp)$/.test(file.type)
          ? URL.createObjectURL(file)
          : null,
      });
    renderAttachments();
  }
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
    const controls = document.querySelector("#turn-controls");
    const busy = sending.has(activeChat);
    const disabled = controls?.dataset.ready !== "true" || busy;
    if (input) input.disabled = disabled;
    const form = document.querySelector("#message-form");
    if (form) {
      for (const el of form.querySelectorAll(
        "#attach-files, #file-input, [data-remove-attachment], .send",
      ))
        el.disabled = disabled;
      form.setAttribute("aria-busy", String(busy));
      const status = form.querySelector(".upload-status");
      if (status) status.hidden = !busy;
    }
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
    if (input) input.value = draft().text;
    renderAttachments();
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
      const submission = submissions.get(ctx.sourceElement);
      if (submission && drafts.get(submission.chat) === submission.draft) {
        releaseDraft(submission.draft);
        drafts.delete(submission.chat);
        if (activeChat === submission.chat) {
          const input = document.querySelector("#message");
          if (input) input.value = "";
          renderAttachments();
        }
      }
    }
  });
  document.addEventListener("htmx:error", () =>
    showError("Could not reach Hypercode. Check the connection and try again."),
  );
  document.addEventListener("input", (event) => {
    if (event.target.id === "message") draft().text = event.target.value;
    if (event.target.matches("[data-free-answer]")) {
      const fieldset = event.target.closest("fieldset");
      fieldset.querySelectorAll("input[type=radio]").forEach((radio) => {
        if (event.target.value) radio.checked = false;
      });
    }
  });

  document.addEventListener("htmx:finally:request", (event) => {
    const form = event.detail.ctx.sourceElement;
    const submission = submissions.get(form);
    if (!submission) return;
    if (sending.get(submission.chat) === submission)
      sending.delete(submission.chat);
    submissions.delete(form);
    ready();
  });
  document.addEventListener("click", (event) => {
    if (event.target.id === "attach-files")
      document.querySelector("#file-input")?.click();
    const remove = event.target.closest?.("[data-remove-attachment]");
    if (remove && !remove.disabled) {
      const [entry] = draft().files.splice(
        Number(remove.dataset.removeAttachment),
        1,
      );
      if (entry?.url) URL.revokeObjectURL(entry.url);
      renderAttachments();
    }
  });
  document.addEventListener("paste", (event) => {
    if (event.target.id !== "message" || event.target.disabled) return;
    const files = Array.from(event.clipboardData?.items || [])
      .filter((item) => item.kind === "file")
      .map((item) => item.getAsFile())
      .filter(Boolean);
    if (!files.length) return;
    event.preventDefault();
    addFiles(files);
    const text = event.clipboardData.getData("text/plain");
    if (text) {
      event.target.setRangeText(
        text,
        event.target.selectionStart,
        event.target.selectionEnd,
        "end",
      );
      draft().text = event.target.value;
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
    if (event.target.id === "file-input") {
      addFiles(event.target.files);
      event.target.value = "";
    }
    if (event.target.matches("input[type=radio]")) {
      const text = event.target
        .closest("fieldset")
        ?.querySelector("[data-free-answer]");
      if (text) text.value = "";
    }
  });
  // Omit empty free-text inputs when a radio answer was selected.
  document.addEventListener("htmx:before:request", (event) => {
    const ctx = event.detail.ctx;
    const body = ctx.request.body;
    if (ctx.sourceElement.id === "message-form") {
      const chat = ctx.sourceElement.dataset.chat;
      const value = draft(chat);
      if (sending.has(chat) || (!value.text.trim() && !value.files.length)) {
        event.preventDefault();
        if (!sending.has(chat))
          showError("Write a message or attach a file first.");
        return;
      }
      if (!(body instanceof FormData)) {
        event.preventDefault();
        showError("Could not prepare the upload.");
        return;
      }
      for (const entry of value.files)
        body.append("files", entry.file, entry.file.name);
      const submission = { chat, draft: value };
      submissions.set(ctx.sourceElement, submission);
      sending.set(chat, submission);
      ready();
    }
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
