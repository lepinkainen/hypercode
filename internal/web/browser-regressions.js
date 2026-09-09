// Run against `task dev-fixture` with playwright-cli run-code --filename.
async (page) => {
  const assert = (ok, message) => { if (!ok) throw new Error(message); };
  await page.unrouteAll({behavior: "ignoreErrors"});
  await page.goto(page.url().split("/").slice(0, 3).join("/") + "/");
  await page.locator("#new-chat-form button").waitFor();
  await page.locator("#harness").selectOption("codex");
  let requests = 0, arrived, release;
  const started = new Promise(resolve => { arrived = resolve; });
  const gate = new Promise(resolve => { release = resolve; });
  await page.route("**/chats", async route => {
    requests++;
    arrived();
    await gate;
    await route.fulfill({status: 204});
  });
  try {
    await page.locator("#new-chat-form button").click();
    await started;
    const disabled = await page.locator("#new-chat-form button").isDisabled();
    await page.evaluate(() => {
      window.requestFinished = new Promise(resolve => document.addEventListener("htmx:finally:request", resolve, {once:true}));
      document.querySelector("#new-chat-form").requestSubmit();
      document.querySelector("#new-chat-form").requestSubmit();
    });
    release();
    await page.evaluate(async () => {
      await window.requestFinished;
      await new Promise(requestAnimationFrame);
    });
    assert(disabled, "Create button stayed enabled during POST");
    assert(requests === 1, `Repeated submission sent ${requests} requests`);
  } finally {
    release();
    await page.unrouteAll({behavior: "wait"});
  }
  await page.locator("#new-chat-form button").click();
  await page.waitForURL("**/chats/*");
  await page.waitForFunction(() => !document.querySelector("#message")?.disabled);
  await page.locator("#message").fill("[reject]");
  await page.locator("#message").press("Enter");
  await page.locator('.item.user[data-status="rejected"]').waitFor();
  await page.waitForFunction(() => !document.querySelector("#message").disabled);
  assert(await page.locator("#message").inputValue() === "[reject]", "Rejected draft was lost");
  assert((await page.locator('.item.user[data-status="rejected"]').innerText()).includes("not sent"), "Rejection was not visible");

  await page.locator("#message").fill("[approval]");
  await page.locator("#message").press("Enter");
  const form = page.locator('.item.approval[data-status="pending"] form');
  await form.waitFor();
  let answers = 0, answerArrived, releaseAnswer;
  const answering = new Promise(resolve => { answerArrived = resolve; });
  const answerGate = new Promise(resolve => { releaseAnswer = resolve; });
  await page.route("**/answer/*", async route => {
    answers++;
    answerArrived();
    await answerGate;
    await route.fulfill({status: 204});
  });
  try {
    await form.getByRole("button", {name: "Allow once", exact: true}).click();
    await answering;
    const disabled = await form.locator("button").evaluateAll(buttons => buttons.every(button => button.disabled));
    await form.evaluate(form => {
      window.answerFinished = new Promise(resolve => document.addEventListener("htmx:finally:request", resolve, {once:true}));
      form.requestSubmit();
    });
    releaseAnswer();
    await page.evaluate(async () => { await window.answerFinished; await new Promise(requestAnimationFrame); });
    assert(disabled, "Some approval buttons stayed enabled during POST");
    assert(answers === 1, `Repeated approval sent ${answers} requests`);
  } finally {
    releaseAnswer();
    await page.unrouteAll({behavior: "wait"});
  }
  await form.getByRole("button", {name: "Allow once", exact: true}).click();
  await page.waitForFunction(() => !document.querySelector("#message").disabled);
  await page.locator('.item.approval[data-status="answered"]').waitFor();

  // Exercise the actual encoded browser POST at the largest BMP message size.
  await page.locator("#message").fill("界".repeat(100000));
  const sent = page.waitForResponse(response => response.url().endsWith("/send") && response.request().method() === "POST");
  await page.locator("#message").press("Enter");
  assert((await sent).status() === 204, "Valid large browser message was rejected");
  await page.waitForFunction(() => !document.querySelector("#message").disabled);

  await page.addInitScript(() => {
    if (window.testStreams) return;
    window.testStreams = [];
    const NativeEventSource = window.EventSource;
    window.EventSource = class extends NativeEventSource {
      constructor(url) { super(url); window.testStreams.push(this); }
    };
  });
  await page.reload();
  await page.waitForFunction(() => !document.querySelector("#message").disabled && window.testStreams.at(-1)?.readyState === 1);
  await page.locator("#message").fill("draft survives cached-page restoration");
  await page.evaluate(() => {
    window.dispatchEvent(new PageTransitionEvent("pagehide", {persisted: true}));
    if (window.testStreams.at(-1).readyState !== 2) throw new Error("pagehide did not close the stream");
    window.dispatchEvent(new PageTransitionEvent("pageshow", {persisted: true}));
  });
  await page.waitForFunction(() => window.testStreams.length === 2 && window.testStreams[1].readyState === 1);
  assert(await page.locator("#message").inputValue() === "draft survives cached-page restoration", "Cached-page restoration lost the draft");
  return "Passed: duplicate create/approval submissions, rejected-send retry, large Unicode POST, and cached-page lifecycle reconnect with draft preservation";
}
