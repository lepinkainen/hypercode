// Run against task dev-fixture with playwright-cli run-code --filename.
async (page) => {
  const assert = (ok, message) => {
    if (!ok) throw new Error(message);
  };
  const base = page.url().split("/").slice(0, 3).join("/");
  await page.unrouteAll({ behavior: "ignoreErrors" });
  await page.goto(base);
  const setFiles = async (files) =>
    page.locator("#file-input").evaluate(
      (input, files) => {
        const transfer = new DataTransfer();
        for (const file of files) {
          const data = file.size
            ? new Uint8Array(file.size)
            : Array.isArray(file.data)
              ? new Uint8Array(file.data)
              : file.data;
          transfer.items.add(
            new File([data], file.name, { type: file.mimeType }),
          );
        }
        input.files = transfer.files;
        input.dispatchEvent(new Event("change", { bubbles: true }));
      },
      Array.isArray(files) ? files : [files],
    );
  const directory = await page.locator("#directory").inputValue();
  const create = async (harness) => {
    const response = await page.request.post(`${base}/chats`, {
      form: { harness, directory, mode: "read-only" },
    });
    assert(
      response.status() === 201,
      `create ${harness}: ${response.status()}`,
    );
    return response.headers().location;
  };
  const first = await create("codex"),
    second = await create("claude");
  const ready = () =>
    page.waitForFunction(
      () =>
        document.querySelector("#message") &&
        !document.querySelector("#message").disabled,
    );
  await page.goto(base + first);
  await ready();
  // Generate a real PNG in the browser, then paste it with accompanying text.
  await page.evaluate(async () => {
    const canvas = document.createElement("canvas");
    canvas.width = 64;
    canvas.height = 64;
    canvas.getContext("2d").fillRect(0, 0, 64, 64);
    const blob = await new Promise((resolve) => canvas.toBlob(resolve));
    window.attachmentPNG = Array.from(new Uint8Array(await blob.arrayBuffer()));
    const clipboard = new DataTransfer();
    clipboard.items.add(new File([blob], "pasted.png", { type: "image/png" }));
    clipboard.setData("text/plain", "Pasted caption");
    document.querySelector("#message").dispatchEvent(
      new ClipboardEvent("paste", {
        bubbles: true,
        cancelable: true,
        clipboardData: clipboard,
      }),
    );
  });
  assert(
    (await page.locator("#message").inputValue()) === "Pasted caption",
    "Mixed clipboard lost text",
  );
  await page.locator("#draft-attachments img").waitFor();
  await setFiles({
    name: "notes.md",
    mimeType: "text/markdown",
    data: "# document fixture",
  });
  assert(
    (await page.locator("#draft-attachments .attachment").count()) === 2,
    "File picker replaced pasted image",
  );
  await page
    .getByRole("button", { name: "Remove notes.md", exact: true })
    .click();
  assert(
    (await page.locator("#draft-attachments .attachment").count()) === 1,
    "Remove failed",
  );
  await page.locator(`#chat-list a[href="${second}"]`).click();
  await page.waitForURL(base + second);
  await ready();
  assert(
    (await page.locator("#draft-attachments .attachment").count()) === 0,
    "Files leaked to another chat",
  );
  await page.locator("#message").fill("Other chat draft");
  await page.locator(`#chat-list a[href="${first}"]`).click();
  await page.waitForURL(base + first);
  await ready();
  assert(
    (await page.locator("#message").inputValue()) === "Pasted caption",
    "Navigation lost text",
  );
  assert(
    (await page.locator("#draft-attachments .attachment").count()) === 1,
    "Navigation lost files",
  );

  // Hold a real upload while navigating. Its completion must clear only its
  // own draft, and its disabled state must survive swapping the composer.
  let release,
    arrived,
    requests = 0;
  const gate = new Promise((resolve) => {
    release = resolve;
  });
  const started = new Promise((resolve) => {
    arrived = resolve;
  });
  await page.route(`**${first}/send`, async (route) => {
    requests++;
    arrived();
    await gate;
    await route.continue();
  });
  try {
    await page.getByRole("button", { name: "Send", exact: true }).click();
    await started;
    assert(
      await page.locator(".upload-status").isVisible(),
      "No upload feedback",
    );
    assert(
      await page
        .getByRole("button", { name: "Send", exact: true })
        .isDisabled(),
      "Send stayed enabled",
    );
    await page.evaluate(() =>
      document.querySelector("#message-form").requestSubmit(),
    );
    await page.locator(`#chat-list a[href="${second}"]`).click();
    await page.waitForURL(base + second);
    await ready();
    const response = page.waitForResponse((r) =>
      r.url().endsWith(first + "/send"),
    );
    release();
    assert((await response).status() === 204, "Upload rejected");
    await page.waitForFunction(
      () => document.querySelector("#message").value === "Other chat draft",
    );
    assert(requests === 1, "Upload submitted more than once");
  } finally {
    release();
    await page.unrouteAll({ behavior: "wait" });
  }
  await page.locator(`#chat-list a[href="${first}"]`).click();
  await page.waitForURL(base + first);
  await ready();
  await page
    .locator(".item.assistant")
    .filter({ hasText: "Received 1 images" })
    .waitFor();
  assert(
    (await page.locator("#draft-attachments .attachment").count()) === 0,
    "Successful detached send retained draft",
  );
  const imageURL = await page
    .locator(".sent-attachments img")
    .getAttribute("src");
  await page.reload();
  await ready();
  assert(
    (await page.locator(".sent-attachments img").count()) === 1,
    "Refresh lost sent image",
  );
  assert(
    (await page.request.get(base + imageURL)).status() === 200,
    "Stored image not served",
  );

  // Rejected sends retain the file and text. Retry an attachment-only turn.
  const png = await page.evaluate(async () =>
    Array.from(
      new Uint8Array(
        await (
          await fetch(document.querySelector(".sent-attachments img").src)
        ).arrayBuffer(),
      ),
    ),
  );
  await setFiles({ name: "retry.png", mimeType: "image/png", data: png });
  await page.locator("#message").fill("[reject]");
  await page.locator("#message").press("Enter");
  await page.locator('.item.user[data-status="rejected"]').waitFor();
  await ready();
  assert(
    (await page.locator("#draft-attachments .attachment").count()) === 1,
    "Rejection lost file",
  );
  assert(
    (await page.locator("#message").inputValue()) === "[reject]",
    "Rejection lost text",
  );
  await page.route(`**${first}/send`, (route) => route.abort("failed"));
  try {
    const failed = page.waitForEvent("requestfailed", (request) =>
      request.url().endsWith(first + "/send"),
    );
    await page.getByRole("button", { name: "Send", exact: true }).click();
    await failed;
    await ready();
    assert(
      (await page.locator("#draft-attachments .attachment").count()) === 1,
      "Network failure lost attachment",
    );
    assert(
      (await page.locator("#message").inputValue()) === "[reject]",
      "Network failure lost text",
    );
  } finally {
    await page.unrouteAll({ behavior: "wait" });
  }
  await page.locator("#message").fill("");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await ready();
  await page.waitForFunction(
    () =>
      document.querySelectorAll("#draft-attachments .attachment").length === 0,
  );

  // Verify client rejection, then exercise Claude's native image block.
  await setFiles({
    name: "oversize.png",
    mimeType: "image/png",
    size: 5000001,
  });
  assert(
    (await page.locator("#draft-attachments .attachment").count()) === 0,
    "Oversize file added",
  );
  assert(
    await page.locator("#app-error").isVisible(),
    "Oversize file has no explanation",
  );
  await page.locator(`#chat-list a[href="${second}"]`).click();
  await page.waitForURL(base + second);
  await ready();
  await setFiles([
    { name: "claude.png", mimeType: "image/png", data: png },
    { name: "notes.md", mimeType: "text/markdown", data: "# document fixture" },
  ]);
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await ready();
  await page
    .locator(".item.assistant")
    .filter({ hasText: "Received 1 images" })
    .waitFor();
  const docURL = await page
    .locator(".sent-attachments a[download]")
    .getAttribute("href");
  const doc = await page.request.get(base + docURL);
  assert(
    doc.headers()["content-disposition"].startsWith("attachment;"),
    "Document served inline",
  );
  assert((await doc.text()) === "# document fixture", "Document bytes changed");
  await page.setViewportSize({ width: 390, height: 844 });
  assert(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
    "Mobile page overflow",
  );
  await page.screenshot({
    path: "output/playwright/attachments-mobile.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.screenshot({
    path: "output/playwright/attachments-desktop.png",
    fullPage: true,
  });
  return "Passed: image paste with text, file selection/removal, per-chat drafts, upload feedback, navigation during send, duplicate prevention, refresh, rejection/retry, network failure, image-only turns, size validation, both native image formats, document download, mobile layout.";
}
